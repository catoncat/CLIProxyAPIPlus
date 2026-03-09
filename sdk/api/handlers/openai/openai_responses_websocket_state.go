package openai

import (
	"bytes"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	responsesWebsocketChainContextTTL             = 1 * time.Hour
	responsesWebsocketChainContextCleanupInterval = 15 * time.Minute
)

type responsesWebsocketChainContext struct {
	AuthID          string
	RequestSnapshot []byte
	FullTranscript  []byte
	LocalPrewarm    bool
	Expire          time.Time
}

var (
	responsesWebsocketChainContextMu   sync.RWMutex
	responsesWebsocketChainContextMap  = make(map[string]responsesWebsocketChainContext)
	responsesWebsocketChainContextOnce sync.Once
)

func startResponsesWebsocketChainContextCleanup() {
	go func() {
		ticker := time.NewTicker(responsesWebsocketChainContextCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			purgeExpiredResponsesWebsocketChainContexts()
		}
	}()
}

func purgeExpiredResponsesWebsocketChainContexts() {
	now := time.Now()

	responsesWebsocketChainContextMu.Lock()
	defer responsesWebsocketChainContextMu.Unlock()

	for responseID, ctx := range responsesWebsocketChainContextMap {
		if ctx.Expire.Before(now) {
			delete(responsesWebsocketChainContextMap, responseID)
		}
	}
}

func getResponsesWebsocketChainContext(responseID string) (responsesWebsocketChainContext, bool) {
	responseID = stringsTrimSpace(responseID)
	if responseID == "" {
		return responsesWebsocketChainContext{}, false
	}
	responsesWebsocketChainContextOnce.Do(startResponsesWebsocketChainContextCleanup)

	responsesWebsocketChainContextMu.RLock()
	ctx, ok := responsesWebsocketChainContextMap[responseID]
	responsesWebsocketChainContextMu.RUnlock()
	if !ok || ctx.Expire.Before(time.Now()) {
		return responsesWebsocketChainContext{}, false
	}
	return cloneResponsesWebsocketChainContext(ctx), true
}

func setResponsesWebsocketChainContext(responseID string, ctx responsesWebsocketChainContext) {
	responseID = stringsTrimSpace(responseID)
	if responseID == "" {
		return
	}
	responsesWebsocketChainContextOnce.Do(startResponsesWebsocketChainContextCleanup)
	ctx.Expire = time.Now().Add(responsesWebsocketChainContextTTL)

	responsesWebsocketChainContextMu.Lock()
	responsesWebsocketChainContextMap[responseID] = cloneResponsesWebsocketChainContext(ctx)
	responsesWebsocketChainContextMu.Unlock()
}

func cloneResponsesWebsocketChainContext(ctx responsesWebsocketChainContext) responsesWebsocketChainContext {
	ctx.RequestSnapshot = bytes.Clone(ctx.RequestSnapshot)
	ctx.FullTranscript = bytes.Clone(ctx.FullTranscript)
	return ctx
}

func restoreResponsesWebsocketChainState(rawJSON []byte, pinnedAuthID *string, lastRequest *[]byte, lastResponseOutput *[]byte, localPrewarmResponseID *string) bool {
	prevResponseID := stringsTrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String())
	if prevResponseID == "" {
		return false
	}

	ctx, ok := getResponsesWebsocketChainContext(prevResponseID)
	if !ok {
		return false
	}

	if pinnedAuthID != nil && stringsTrimSpace(*pinnedAuthID) == "" && ctx.AuthID != "" {
		*pinnedAuthID = ctx.AuthID
	}
	if lastRequest != nil && len(*lastRequest) == 0 && len(ctx.RequestSnapshot) > 0 {
		*lastRequest = bytes.Clone(ctx.RequestSnapshot)
	}
	if lastResponseOutput != nil && len(*lastResponseOutput) == 0 {
		*lastResponseOutput = []byte("[]")
	}
	if localPrewarmResponseID != nil && ctx.LocalPrewarm {
		*localPrewarmResponseID = prevResponseID
	}
	return true
}

func storeResponsesWebsocketChainState(responseID string, authID string, requestJSON []byte, completedOutput []byte, localPrewarm bool) bool {
	responseID = stringsTrimSpace(responseID)
	if responseID == "" || len(requestJSON) == 0 {
		return false
	}

	fullTranscript, ok := buildResponsesWebsocketFullTranscript(requestJSON, completedOutput, localPrewarm)
	if !ok {
		return false
	}
	requestSnapshot, ok := buildResponsesWebsocketRequestSnapshot(requestJSON, fullTranscript)
	if !ok {
		return false
	}

	setResponsesWebsocketChainContext(responseID, responsesWebsocketChainContext{
		AuthID:          stringsTrimSpace(authID),
		RequestSnapshot: requestSnapshot,
		FullTranscript:  fullTranscript,
		LocalPrewarm:    localPrewarm,
	})
	return true
}

func buildResponsesWebsocketFullTranscript(requestJSON []byte, completedOutput []byte, localPrewarm bool) ([]byte, bool) {
	requestInput := normalizeJSONArrayRaw([]byte(gjson.GetBytes(requestJSON, "input").Raw))
	transcript := requestInput

	if !localPrewarm {
		if prevResponseID := stringsTrimSpace(gjson.GetBytes(requestJSON, "previous_response_id").String()); prevResponseID != "" {
			prevCtx, ok := getResponsesWebsocketChainContext(prevResponseID)
			if !ok || len(prevCtx.FullTranscript) == 0 {
				return nil, false
			}
			merged, errMerge := mergeJSONArrayRaw(string(prevCtx.FullTranscript), requestInput)
			if errMerge != nil {
				return nil, false
			}
			transcript = merged
		}
		merged, errMerge := mergeJSONArrayRaw(transcript, normalizeJSONArrayRaw(completedOutput))
		if errMerge != nil {
			return nil, false
		}
		transcript = merged
	}

	return bytes.Clone([]byte(transcript)), true
}

func buildResponsesWebsocketRequestSnapshot(requestJSON []byte, fullTranscript []byte) ([]byte, bool) {
	if len(requestJSON) == 0 {
		return nil, false
	}
	snapshot := bytes.Clone(requestJSON)
	snapshot, _ = sjson.DeleteBytes(snapshot, "previous_response_id")
	updated, errSet := sjson.SetRawBytes(snapshot, "input", bytes.Clone(fullTranscript))
	if errSet != nil {
		return nil, false
	}
	updated, _ = sjson.SetBytes(updated, "stream", true)
	return updated, true
}

func shouldFallbackPreviousResponseNotFound(errMsg *interfaces.ErrorMessage, sentPayload bool) bool {
	if errMsg == nil || sentPayload || errMsg.Error == nil {
		return false
	}
	raw := stringsTrimSpace(errMsg.Error.Error())
	if raw == "" || !gjson.Valid(raw) {
		return false
	}
	return stringsTrimSpace(gjson.Get(raw, "error.code").String()) == "previous_response_not_found"
}

func stringsTrimSpace(raw string) string {
	return strings.TrimSpace(raw)
}
