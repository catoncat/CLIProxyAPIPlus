package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	wsRequestTypeCreate  = "response.create"
	wsRequestTypeAppend  = "response.append"
	wsEventTypeError     = "error"
	wsEventTypeCreated   = "response.created"
	wsEventTypeCompleted = "response.completed"
	wsEventTypeDone      = "response.done"
	wsDoneMarker         = "[DONE]"
	wsTurnStateHeader    = "x-codex-turn-state"
	wsOpenAIBetaHeader   = "OpenAI-Beta"
	wsRequestBodyKey     = "REQUEST_BODY_OVERRIDE"
	wsPayloadLogMaxSize  = 2048
)

var responsesWebsocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// responsesWebsocketTelemetry keeps process-level counters to help diagnose
// prompt-cache effectiveness and websocket prewarm behavior regressions.
var responsesWebsocketTelemetry struct {
	localPrewarmHandled    atomic.Int64
	upstreamPrewarmHandled atomic.Int64
	prevResponseIDDropped  atomic.Int64
	responseCompletedSeen  atomic.Int64
	cachedTokensTotal      atomic.Int64
	inputTokensTotal       atomic.Int64
	totalTokensTotal       atomic.Int64
}

type websocketSessionTelemetry struct {
	localPrewarmHandled    int64
	upstreamPrewarmHandled int64
	prevResponseIDDropped  int64
	responseCompletedSeen  int64
	cachedTokensTotal      int64
	inputTokensTotal       int64
	totalTokensTotal       int64
}

func (t *websocketSessionTelemetry) onLocalPrewarm() {
	if t == nil {
		return
	}
	t.localPrewarmHandled++
	responsesWebsocketTelemetry.localPrewarmHandled.Add(1)
}

func (t *websocketSessionTelemetry) onUpstreamPrewarm() {
	if t == nil {
		return
	}
	t.upstreamPrewarmHandled++
	responsesWebsocketTelemetry.upstreamPrewarmHandled.Add(1)
}

func (t *websocketSessionTelemetry) onPreviousResponseIDDropped() {
	if t == nil {
		return
	}
	t.prevResponseIDDropped++
	responsesWebsocketTelemetry.prevResponseIDDropped.Add(1)
}

func (t *websocketSessionTelemetry) onCompletedPayload(payload []byte) {
	if t == nil {
		return
	}
	t.responseCompletedSeen++
	responsesWebsocketTelemetry.responseCompletedSeen.Add(1)

	cachedTokens := gjson.GetBytes(payload, "response.usage.input_tokens_details.cached_tokens").Int()
	inputTokens := gjson.GetBytes(payload, "response.usage.input_tokens").Int()
	totalTokens := gjson.GetBytes(payload, "response.usage.total_tokens").Int()

	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if inputTokens < 0 {
		inputTokens = 0
	}
	if totalTokens < 0 {
		totalTokens = 0
	}

	t.cachedTokensTotal += cachedTokens
	t.inputTokensTotal += inputTokens
	t.totalTokensTotal += totalTokens

	responsesWebsocketTelemetry.cachedTokensTotal.Add(cachedTokens)
	responsesWebsocketTelemetry.inputTokensTotal.Add(inputTokens)
	responsesWebsocketTelemetry.totalTokensTotal.Add(totalTokens)
}

func (t *websocketSessionTelemetry) cacheHitRatePercent() float64 {
	if t == nil || t.inputTokensTotal <= 0 {
		return 0
	}
	return float64(t.cachedTokensTotal) * 100 / float64(t.inputTokensTotal)
}

func telemetryCacheHitRatePercent(cachedTokens, inputTokens int64) float64 {
	if inputTokens <= 0 {
		return 0
	}
	return float64(cachedTokens) * 100 / float64(inputTokens)
}

// ResponsesWebsocket handles websocket requests for /v1/responses.
// It accepts `response.create` and `response.append` requests and streams
// response events back as JSON websocket text messages.
func (h *OpenAIResponsesAPIHandler) ResponsesWebsocket(c *gin.Context) {
	conn, err := responsesWebsocketUpgrader.Upgrade(c.Writer, c.Request, websocketUpgradeHeaders(c.Request))
	if err != nil {
		return
	}
	passthroughSessionID := uuid.NewString()
	clientRemoteAddr := ""
	useCompletedEvents := websocketClientPrefersCompleted(c.Request)
	if c != nil && c.Request != nil {
		clientRemoteAddr = strings.TrimSpace(c.Request.RemoteAddr)
	}
	log.Infof("responses websocket: client connected id=%s remote=%s", passthroughSessionID, clientRemoteAddr)
	var wsTerminateErr error
	var wsBodyLog strings.Builder
	var wsMetrics websocketSessionTelemetry
	defer func() {
		globalLocalPrewarm := responsesWebsocketTelemetry.localPrewarmHandled.Load()
		globalUpstreamPrewarm := responsesWebsocketTelemetry.upstreamPrewarmHandled.Load()
		globalPrevDropped := responsesWebsocketTelemetry.prevResponseIDDropped.Load()
		globalCompleted := responsesWebsocketTelemetry.responseCompletedSeen.Load()
		globalCached := responsesWebsocketTelemetry.cachedTokensTotal.Load()
		globalInput := responsesWebsocketTelemetry.inputTokensTotal.Load()
		globalTotal := responsesWebsocketTelemetry.totalTokensTotal.Load()
		log.Infof(
			"responses websocket telemetry: session_id=%s local_prewarm=%d upstream_prewarm=%d prev_response_id_dropped=%d completed_events=%d cached_tokens=%d input_tokens=%d total_tokens=%d cache_hit_rate=%.2f%% global_local_prewarm=%d global_upstream_prewarm=%d global_prev_response_id_dropped=%d global_completed_events=%d global_cached_tokens=%d global_input_tokens=%d global_total_tokens=%d global_cache_hit_rate=%.2f%%",
			passthroughSessionID,
			wsMetrics.localPrewarmHandled,
			wsMetrics.upstreamPrewarmHandled,
			wsMetrics.prevResponseIDDropped,
			wsMetrics.responseCompletedSeen,
			wsMetrics.cachedTokensTotal,
			wsMetrics.inputTokensTotal,
			wsMetrics.totalTokensTotal,
			wsMetrics.cacheHitRatePercent(),
			globalLocalPrewarm,
			globalUpstreamPrewarm,
			globalPrevDropped,
			globalCompleted,
			globalCached,
			globalInput,
			globalTotal,
			telemetryCacheHitRatePercent(globalCached, globalInput),
		)

		if wsTerminateErr != nil {
			// log.Infof("responses websocket: session closing id=%s reason=%v", passthroughSessionID, wsTerminateErr)
		} else {
			log.Infof("responses websocket: session closing id=%s", passthroughSessionID)
		}
		if h != nil && h.AuthManager != nil {
			h.AuthManager.CloseExecutionSession(passthroughSessionID)
			log.Infof("responses websocket: upstream execution session closed id=%s", passthroughSessionID)
		}
		setWebsocketRequestBody(c, wsBodyLog.String())
		if errClose := conn.Close(); errClose != nil {
			log.Warnf("responses websocket: close connection error: %v", errClose)
		}
	}()

	var lastRequest []byte
	lastResponseOutput := []byte("[]")
	pinnedAuthID := ""

	for {
		msgType, payload, errReadMessage := conn.ReadMessage()
		if errReadMessage != nil {
			wsTerminateErr = errReadMessage
			appendWebsocketEvent(&wsBodyLog, "disconnect", []byte(errReadMessage.Error()))
			if websocket.IsCloseError(errReadMessage, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				log.Infof("responses websocket: client disconnected id=%s error=%v", passthroughSessionID, errReadMessage)
			} else {
				// log.Warnf("responses websocket: read message failed id=%s error=%v", passthroughSessionID, errReadMessage)
			}
			return
		}
		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}
		// log.Infof(
		// 	"responses websocket: downstream_in id=%s type=%d event=%s payload=%s",
		// 	passthroughSessionID,
		// 	msgType,
		// 	websocketPayloadEventType(payload),
		// 	websocketPayloadPreview(payload),
		// )
		appendWebsocketEvent(&wsBodyLog, "request", payload)

		allowIncrementalInputWithPreviousResponseID := false
		if pinnedAuthID != "" && h != nil && h.AuthManager != nil {
			if pinnedAuth, ok := h.AuthManager.GetByID(pinnedAuthID); ok && pinnedAuth != nil {
				allowIncrementalInputWithPreviousResponseID = websocketUpstreamSupportsIncrementalInput(pinnedAuth.Attributes, pinnedAuth.Metadata)
			}
		} else {
			requestModelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
			if requestModelName == "" {
				requestModelName = strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
			}
			allowIncrementalInputWithPreviousResponseID = h.websocketUpstreamSupportsIncrementalInputForModel(requestModelName)
		}

		var requestJSON []byte
		var updatedLastRequest []byte
		var errMsg *interfaces.ErrorMessage
		requestJSON, updatedLastRequest, errMsg = normalizeResponsesWebsocketRequestWithMode(
			payload,
			lastRequest,
			lastResponseOutput,
			allowIncrementalInputWithPreviousResponseID,
			&wsMetrics,
		)
		if errMsg != nil {
			h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
			markAPIResponseTimestamp(c)
			errorPayload, errWrite := writeResponsesWebsocketError(conn, errMsg)
			appendWebsocketEvent(&wsBodyLog, "response", errorPayload)
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				passthroughSessionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
			if errWrite != nil {
				log.Warnf(
					"responses websocket: downstream_out write failed id=%s event=%s error=%v",
					passthroughSessionID,
					websocketPayloadEventType(errorPayload),
					errWrite,
				)
				return
			}
			continue
		}
		if shouldHandleResponsesWebsocketPrewarmLocally(payload, lastRequest, allowIncrementalInputWithPreviousResponseID) {
			wsMetrics.onLocalPrewarm()
			if updated, errDelete := sjson.DeleteBytes(requestJSON, "generate"); errDelete == nil {
				requestJSON = updated
			}
			if updated, errDelete := sjson.DeleteBytes(updatedLastRequest, "generate"); errDelete == nil {
				updatedLastRequest = updated
			}
			lastRequest = updatedLastRequest
			_, payloads := buildResponsesWebsocketPrewarmPayloads(requestJSON, useCompletedEvents)
			lastResponseOutput = []byte("[]")
			for _, syntheticPayload := range payloads {
				markAPIResponseTimestamp(c)
				appendWebsocketEvent(&wsBodyLog, "response", syntheticPayload)
				if errWrite := conn.WriteMessage(websocket.TextMessage, syntheticPayload); errWrite != nil {
					wsTerminateErr = errWrite
					appendWebsocketEvent(&wsBodyLog, "disconnect", []byte(errWrite.Error()))
					log.Warnf(
						"responses websocket: downstream_out write failed id=%s event=%s error=%v",
						passthroughSessionID,
						websocketPayloadEventType(syntheticPayload),
						errWrite,
					)
					return
				}
			}
			continue
		}
		if isResponsesWebsocketPrewarmRequest(payload) {
			wsMetrics.onUpstreamPrewarm()
		}
		lastRequest = updatedLastRequest

		modelName := gjson.GetBytes(requestJSON, "model").String()
		cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
		cliCtx = cliproxyexecutor.WithDownstreamWebsocket(cliCtx)
		cliCtx = handlers.WithExecutionSessionID(cliCtx, passthroughSessionID)
		if pinnedAuthID != "" {
			cliCtx = handlers.WithPinnedAuthID(cliCtx, pinnedAuthID)
		} else {
			cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, func(authID string) {
				pinnedAuthID = strings.TrimSpace(authID)
			})
		}
		dataChan, _, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, requestJSON, "")

		completedOutput, errForward := h.forwardResponsesWebsocket(c, conn, cliCancel, dataChan, errChan, &wsBodyLog, passthroughSessionID, useCompletedEvents, &wsMetrics)
		if errForward != nil {
			wsTerminateErr = errForward
			appendWebsocketEvent(&wsBodyLog, "disconnect", []byte(errForward.Error()))
			log.Warnf("responses websocket: forward failed id=%s error=%v", passthroughSessionID, errForward)
			return
		}
		lastResponseOutput = completedOutput
	}
}

func websocketClientPrefersCompleted(req *http.Request) bool {
	if req == nil {
		return false
	}
	betaHeader := strings.TrimSpace(req.Header.Get(wsOpenAIBetaHeader))
	return betaHeader != "" && strings.Contains(betaHeader, "responses_websockets=")
}

func websocketUpgradeHeaders(req *http.Request) http.Header {
	headers := http.Header{}
	if req == nil {
		return headers
	}

	// Keep the same sticky turn-state across reconnects when provided by the client.
	turnState := strings.TrimSpace(req.Header.Get(wsTurnStateHeader))
	if turnState != "" {
		headers.Set(wsTurnStateHeader, turnState)
	}
	return headers
}

func normalizeResponsesWebsocketRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithMode(rawJSON, lastRequest, lastResponseOutput, true, nil)
}

func normalizeResponsesWebsocketRequestWithMode(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, allowIncrementalInputWithPreviousResponseID bool, metrics *websocketSessionTelemetry) ([]byte, []byte, *interfaces.ErrorMessage) {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	switch requestType {
	case wsRequestTypeCreate:
		// log.Infof("responses websocket: response.create request")
		if len(lastRequest) == 0 {
			return normalizeResponseCreateRequest(rawJSON)
		}
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, allowIncrementalInputWithPreviousResponseID, metrics)
	case wsRequestTypeAppend:
		// log.Infof("responses websocket: response.append request")
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, allowIncrementalInputWithPreviousResponseID, metrics)
	default:
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("unsupported websocket request type: %s", requestType),
		}
	}
}

func normalizeResponseCreateRequest(rawJSON []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	if !gjson.GetBytes(normalized, "input").Exists() {
		normalized, _ = sjson.SetRawBytes(normalized, "input", []byte("[]"))
	}

	modelName := strings.TrimSpace(gjson.GetBytes(normalized, "model").String())
	if modelName == "" {
		return nil, nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("missing model in response.create request"),
		}
	}
	return normalized, bytes.Clone(normalized), nil
}

func isResponsesWebsocketPrewarmRequest(rawJSON []byte) bool {
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String()) != wsRequestTypeCreate {
		return false
	}
	if prev := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()); prev != "" {
		return false
	}
	generate := gjson.GetBytes(rawJSON, "generate")
	return generate.Exists() && !generate.Bool()
}

func (h *OpenAIResponsesAPIHandler) websocketUpstreamSupportsIncrementalInputForModel(modelName string) bool {
	if h == nil || h.AuthManager == nil {
		return false
	}

	resolvedModelName := modelName
	initialSuffix := thinking.ParseSuffix(modelName)
	if initialSuffix.ModelName == "auto" {
		resolvedBase := util.ResolveAutoModel(initialSuffix.ModelName)
		if initialSuffix.HasSuffix {
			resolvedModelName = fmt.Sprintf("%s(%s)", resolvedBase, initialSuffix.RawSuffix)
		} else {
			resolvedModelName = resolvedBase
		}
	} else {
		resolvedModelName = util.ResolveAutoModel(modelName)
	}

	parsed := thinking.ParseSuffix(resolvedModelName)
	baseModel := strings.TrimSpace(parsed.ModelName)
	providers := util.GetProviderName(baseModel)
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = util.GetProviderName(resolvedModelName)
	}
	if len(providers) == 0 {
		return false
	}

	providerSet := make(map[string]struct{}, len(providers))
	for i := 0; i < len(providers); i++ {
		providerKey := strings.TrimSpace(strings.ToLower(providers[i]))
		if providerKey == "" {
			continue
		}
		providerSet[providerKey] = struct{}{}
	}
	if len(providerSet) == 0 {
		return false
	}

	modelKey := baseModel
	if modelKey == "" {
		modelKey = strings.TrimSpace(resolvedModelName)
	}
	registryRef := registry.GetGlobalRegistry()
	now := time.Now()
	auths := h.AuthManager.List()
	for i := 0; i < len(auths); i++ {
		auth := auths[i]
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(auth.ID, modelKey) {
			continue
		}
		if !responsesWebsocketAuthAvailableForModel(auth, modelKey, now) {
			continue
		}
		if websocketUpstreamSupportsIncrementalInput(auth.Attributes, auth.Metadata) {
			return true
		}
	}
	return false
}

func responsesWebsocketAuthAvailableForModel(auth *coreauth.Auth, modelName string, now time.Time) bool {
	if auth == nil {
		return false
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	if modelName != "" && len(auth.ModelStates) > 0 {
		state, ok := auth.ModelStates[modelName]
		if (!ok || state == nil) && modelName != "" {
			baseModel := strings.TrimSpace(thinking.ParseSuffix(modelName).ModelName)
			if baseModel != "" && baseModel != modelName {
				state, ok = auth.ModelStates[baseModel]
			}
		}
		if ok && state != nil {
			if state.Status == coreauth.StatusDisabled {
				return false
			}
			if state.Unavailable && !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now) {
				return false
			}
			return true
		}
	}
	if auth.Unavailable && !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now) {
		return false
	}
	return true
}

func shouldHandleResponsesWebsocketPrewarmLocally(rawJSON []byte, lastRequest []byte, allowIncrementalInputWithPreviousResponseID bool) bool {
	if allowIncrementalInputWithPreviousResponseID || len(lastRequest) != 0 {
		return false
	}
	return isResponsesWebsocketPrewarmRequest(rawJSON)
}

func buildResponsesWebsocketPrewarmPayloads(requestJSON []byte, useCompletedEvents bool) (string, [][]byte) {
	responseID := "resp_prewarm_" + uuid.NewString()
	createdAt := time.Now().Unix()

	created := map[string]any{
		"type": wsEventTypeCreated,
		"response": map[string]any{
			"id":         responseID,
			"object":     "response",
			"created_at": createdAt,
			"status":     "in_progress",
			"background": false,
			"error":      nil,
			"output":     []any{},
		},
	}
	if modelName := strings.TrimSpace(gjson.GetBytes(requestJSON, "model").String()); modelName != "" {
		created["response"].(map[string]any)["model"] = modelName
	}

	done := map[string]any{
		"type": wsEventTypeDone,
		"response": map[string]any{
			"id":                 responseID,
			"object":             "response",
			"created_at":         createdAt,
			"status":             "completed",
			"background":         false,
			"error":              nil,
			"incomplete_details": nil,
			"output":             []any{},
			"usage": map[string]any{
				"input_tokens":          0,
				"input_tokens_details":  map[string]any{"cached_tokens": 0},
				"output_tokens":         0,
				"output_tokens_details": map[string]any{"reasoning_tokens": 0},
				"total_tokens":          0,
			},
		},
	}
	if modelName := strings.TrimSpace(gjson.GetBytes(requestJSON, "model").String()); modelName != "" {
		done["response"].(map[string]any)["model"] = modelName
	}

	createdJSON, _ := json.Marshal(created)
	completedJSON, _ := json.Marshal(done)
	if useCompletedEvents {
		completedJSON, _ = sjson.SetBytes(completedJSON, "type", wsEventTypeCompleted)
	}
	return responseID, [][]byte{createdJSON, completedJSON}
}

func normalizeResponseSubsequentRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, allowIncrementalInputWithPreviousResponseID bool, metrics *websocketSessionTelemetry) ([]byte, []byte, *interfaces.ErrorMessage) {
	if len(lastRequest) == 0 {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request received before response.create"),
		}
	}

	nextInput := gjson.GetBytes(rawJSON, "input")
	if !nextInput.Exists() || !nextInput.IsArray() {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request requires array field: input"),
		}
	}

	// Websocket v2 mode uses response.create with previous_response_id + incremental input.
	// Do not expand it into a full input transcript; upstream expects the incremental payload.
	if allowIncrementalInputWithPreviousResponseID {
		if prev := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()); prev != "" {
			lastRequestWasLocalPrewarm := gjson.GetBytes(lastRequest, "generate").Exists() && !gjson.GetBytes(lastRequest, "generate").Bool()
			normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
			if errDelete != nil {
				normalized = bytes.Clone(rawJSON)
			}
			if lastRequestWasLocalPrewarm {
				// The previous response ID came from a synthetic local prewarm response.
				// Upstream never observed that response, so forwarding its ID is invalid.
				// Keep the incremental input, but drop previous_response_id and let the
				// next upstream turn start as a normal response.create.
				normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
				if metrics != nil {
					metrics.onPreviousResponseIDDropped()
				}
			}
			if !gjson.GetBytes(normalized, "model").Exists() {
				modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
				if modelName != "" {
					normalized, _ = sjson.SetBytes(normalized, "model", modelName)
				}
			}
			if !gjson.GetBytes(normalized, "instructions").Exists() {
				instructions := gjson.GetBytes(lastRequest, "instructions")
				if instructions.Exists() {
					normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
				}
			}
			normalized, _ = sjson.SetBytes(normalized, "stream", true)
			return normalized, bytes.Clone(normalized), nil
		}
	}

	existingInput := gjson.GetBytes(lastRequest, "input")
	mergedInput, errMerge := mergeJSONArrayRaw(existingInput.Raw, normalizeJSONArrayRaw(lastResponseOutput))
	if errMerge != nil {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("invalid previous response output: %w", errMerge),
		}
	}

	mergedInput, errMerge = mergeJSONArrayRaw(mergedInput, nextInput.Raw)
	if errMerge != nil {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("invalid request input: %w", errMerge),
		}
	}

	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	var errSet error
	normalized, errSet = sjson.SetRawBytes(normalized, "input", []byte(mergedInput))
	if errSet != nil {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("failed to merge websocket input: %w", errSet),
		}
	}
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	return normalized, bytes.Clone(normalized), nil
}

func websocketUpstreamSupportsIncrementalInput(attributes map[string]string, metadata map[string]any) bool {
	if len(attributes) > 0 {
		if raw := strings.TrimSpace(attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(metadata) == 0 {
		return false
	}
	raw, ok := metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(value))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

func mergeJSONArrayRaw(existingRaw, appendRaw string) (string, error) {
	existingRaw = strings.TrimSpace(existingRaw)
	appendRaw = strings.TrimSpace(appendRaw)
	if existingRaw == "" {
		existingRaw = "[]"
	}
	if appendRaw == "" {
		appendRaw = "[]"
	}

	var existing []json.RawMessage
	if err := json.Unmarshal([]byte(existingRaw), &existing); err != nil {
		return "", err
	}
	var appendItems []json.RawMessage
	if err := json.Unmarshal([]byte(appendRaw), &appendItems); err != nil {
		return "", err
	}

	merged := append(existing, appendItems...)
	out, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func normalizeJSONArrayRaw(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "[]"
	}
	result := gjson.Parse(trimmed)
	if result.Type == gjson.JSON && result.IsArray() {
		return trimmed
	}
	return "[]"
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocket(
	c *gin.Context,
	conn *websocket.Conn,
	cancel handlers.APIHandlerCancelFunc,
	data <-chan []byte,
	errs <-chan *interfaces.ErrorMessage,
	wsBodyLog *strings.Builder,
	sessionID string,
	useCompletedEvents bool,
	metrics *websocketSessionTelemetry,
) ([]byte, error) {
	completed := false
	completedOutput := []byte("[]")

	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return completedOutput, c.Request.Context().Err()
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errMsg != nil {
				h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
				markAPIResponseTimestamp(c)
				errorPayload, errWrite := writeResponsesWebsocketError(conn, errMsg)
				appendWebsocketEvent(wsBodyLog, "response", errorPayload)
				log.Infof(
					"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
					sessionID,
					websocket.TextMessage,
					websocketPayloadEventType(errorPayload),
					websocketPayloadPreview(errorPayload),
				)
				if errWrite != nil {
					// log.Warnf(
					// 	"responses websocket: downstream_out write failed id=%s event=%s error=%v",
					// 	sessionID,
					// 	websocketPayloadEventType(errorPayload),
					// 	errWrite,
					// )
					cancel(errMsg.Error)
					return completedOutput, errWrite
				}
			}
			if errMsg != nil {
				cancel(errMsg.Error)
			} else {
				cancel(nil)
			}
			return completedOutput, nil
		case chunk, ok := <-data:
			if !ok {
				if !completed {
					errMsg := &interfaces.ErrorMessage{
						StatusCode: http.StatusRequestTimeout,
						Error:      fmt.Errorf("stream closed before response.completed"),
					}
					h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
					markAPIResponseTimestamp(c)
					errorPayload, errWrite := writeResponsesWebsocketError(conn, errMsg)
					appendWebsocketEvent(wsBodyLog, "response", errorPayload)
					log.Infof(
						"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
						sessionID,
						websocket.TextMessage,
						websocketPayloadEventType(errorPayload),
						websocketPayloadPreview(errorPayload),
					)
					if errWrite != nil {
						log.Warnf(
							"responses websocket: downstream_out write failed id=%s event=%s error=%v",
							sessionID,
							websocketPayloadEventType(errorPayload),
							errWrite,
						)
						cancel(errMsg.Error)
						return completedOutput, errWrite
					}
					cancel(errMsg.Error)
					return completedOutput, nil
				}
				cancel(nil)
				return completedOutput, nil
			}

			payloads := websocketJSONPayloadsFromChunk(chunk)
			for i := range payloads {
				eventType := gjson.GetBytes(payloads[i], "type").String()
				if eventType == wsEventTypeCompleted {
					completed = true
					completedOutput = responseCompletedOutputFromPayload(payloads[i])
					if metrics != nil {
						metrics.onCompletedPayload(payloads[i])
					}
					if !useCompletedEvents {
						payloads[i], _ = sjson.SetBytes(payloads[i], "type", wsEventTypeDone)
					}
				}
				markAPIResponseTimestamp(c)
				appendWebsocketEvent(wsBodyLog, "response", payloads[i])
				// log.Infof(
				// 	"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				// 	sessionID,
				// 	websocket.TextMessage,
				// 	websocketPayloadEventType(payloads[i]),
				// 	websocketPayloadPreview(payloads[i]),
				// )
				if errWrite := conn.WriteMessage(websocket.TextMessage, payloads[i]); errWrite != nil {
					log.Warnf(
						"responses websocket: downstream_out write failed id=%s event=%s error=%v",
						sessionID,
						websocketPayloadEventType(payloads[i]),
						errWrite,
					)
					cancel(errWrite)
					return completedOutput, errWrite
				}
			}
		}
	}
}

func responseCompletedOutputFromPayload(payload []byte) []byte {
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && output.IsArray() {
		return bytes.Clone([]byte(output.Raw))
	}
	return []byte("[]")
}

func websocketJSONPayloadsFromChunk(chunk []byte) [][]byte {
	payloads := make([][]byte, 0, 2)
	lines := bytes.Split(chunk, []byte("\n"))
	for i := range lines {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[len("data:"):])
		}
		if len(line) == 0 || bytes.Equal(line, []byte(wsDoneMarker)) {
			continue
		}
		if json.Valid(line) {
			payloads = append(payloads, bytes.Clone(line))
		}
	}

	if len(payloads) > 0 {
		return payloads
	}

	trimmed := bytes.TrimSpace(chunk)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte(wsDoneMarker)) && json.Valid(trimmed) {
		payloads = append(payloads, bytes.Clone(trimmed))
	}
	return payloads
}

func writeResponsesWebsocketError(conn *websocket.Conn, errMsg *interfaces.ErrorMessage) ([]byte, error) {
	status := http.StatusInternalServerError
	errText := http.StatusText(status)
	if errMsg != nil {
		if errMsg.StatusCode > 0 {
			status = errMsg.StatusCode
			errText = http.StatusText(status)
		}
		if errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
			errText = errMsg.Error.Error()
		}
	}

	body := handlers.BuildErrorResponseBody(status, errText)
	payload := map[string]any{
		"type":   wsEventTypeError,
		"status": status,
	}

	if errMsg != nil && errMsg.Addon != nil {
		headers := map[string]any{}
		for key, values := range errMsg.Addon {
			if len(values) == 0 {
				continue
			}
			headers[key] = values[0]
		}
		if len(headers) > 0 {
			payload["headers"] = headers
		}
	}

	if len(body) > 0 && json.Valid(body) {
		var decoded map[string]any
		if errDecode := json.Unmarshal(body, &decoded); errDecode == nil {
			if inner, ok := decoded["error"]; ok {
				payload["error"] = inner
			} else {
				payload["error"] = decoded
			}
		}
	}

	if _, ok := payload["error"]; !ok {
		payload["error"] = map[string]any{
			"type":    "server_error",
			"message": errText,
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, conn.WriteMessage(websocket.TextMessage, data)
}

func appendWebsocketEvent(builder *strings.Builder, eventType string, payload []byte) {
	if builder == nil {
		return
	}
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.WriteString("websocket.")
	builder.WriteString(eventType)
	builder.WriteString("\n")
	builder.Write(trimmedPayload)
	builder.WriteString("\n")
}

func websocketPayloadEventType(payload []byte) string {
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if eventType == "" {
		return "-"
	}
	return eventType
}

func websocketPayloadPreview(payload []byte) string {
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return "<empty>"
	}
	preview := trimmedPayload
	if len(preview) > wsPayloadLogMaxSize {
		preview = preview[:wsPayloadLogMaxSize]
	}
	previewText := strings.ReplaceAll(string(preview), "\n", "\\n")
	previewText = strings.ReplaceAll(previewText, "\r", "\\r")
	if len(trimmedPayload) > wsPayloadLogMaxSize {
		return fmt.Sprintf("%s...(truncated,total=%d)", previewText, len(trimmedPayload))
	}
	return previewText
}

func setWebsocketRequestBody(c *gin.Context, body string) {
	if c == nil {
		return
	}
	trimmedBody := strings.TrimSpace(body)
	if trimmedBody == "" {
		return
	}
	c.Set(wsRequestBodyKey, []byte(trimmedBody))
}

func markAPIResponseTimestamp(c *gin.Context) {
	if c == nil {
		return
	}
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); exists {
		return
	}
	c.Set("API_RESPONSE_TIMESTAMP", time.Now())
}
