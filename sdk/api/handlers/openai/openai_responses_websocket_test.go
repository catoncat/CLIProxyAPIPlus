package openai

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

type websocketCaptureExecutor struct {
	mu          sync.Mutex
	calls       int
	lastPayload []byte
}

type previousResponseNotFoundError struct{}

func (previousResponseNotFoundError) Error() string {
	return `{"error":{"code":"previous_response_not_found","message":"previous response not found","param":"previous_response_id","type":"invalid_request_error"}}`
}

func (previousResponseNotFoundError) StatusCode() int { return http.StatusBadRequest }

type websocketRetryExecutor struct {
	mu       sync.Mutex
	payloads [][]byte
}

func (e *websocketCaptureExecutor) Identifier() string { return "test-provider" }

func (e *websocketCaptureExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketCaptureExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	e.lastPayload = append(e.lastPayload[:0], req.Payload...)
	e.mu.Unlock()

	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-upstream\",\"output\":[]}}\n\ndata: [DONE]\n")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *websocketCaptureExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketCaptureExecutor) snapshot() (int, []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls, bytes.Clone(e.lastPayload)
}

func (e *websocketRetryExecutor) Identifier() string { return "test-provider-retry" }

func (e *websocketRetryExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketRetryExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.payloads = append(e.payloads, bytes.Clone(req.Payload))
	callCount := len(e.payloads)
	e.mu.Unlock()

	chunks := make(chan coreexecutor.StreamChunk, 1)
	if callCount == 1 {
		chunks <- coreexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}]}}\n\ndata: [DONE]\n")}
		close(chunks)
		return &coreexecutor.StreamResult{Chunks: chunks}, nil
	}
	if callCount == 2 {
		chunks <- coreexecutor.StreamChunk{Err: previousResponseNotFoundError{}}
		close(chunks)
		return &coreexecutor.StreamResult{Chunks: chunks}, nil
	}

	chunks <- coreexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"fixed\"}]}]}}\n\ndata: [DONE]\n")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *websocketRetryExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketRetryExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketRetryExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketRetryExecutor) snapshot() [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]byte, len(e.payloads))
	for i := range e.payloads {
		out[i] = bytes.Clone(e.payloads[i])
	}
	return out
}

func TestNormalizeResponsesWebsocketRequestCreate(t *testing.T) {
	raw := []byte(`{"type":"response.create","model":"test-model","stream":false,"input":[{"type":"message","id":"msg-1"}]}`)

	normalized, last, errMsg := normalizeResponsesWebsocketRequest(raw, nil, nil)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "type").Exists() {
		t.Fatalf("normalized create request must not include type field")
	}
	if !gjson.GetBytes(normalized, "stream").Bool() {
		t.Fatalf("normalized create request must force stream=true")
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("unexpected model: %s", gjson.GetBytes(normalized, "model").String())
	}
	if !bytes.Equal(last, normalized) {
		t.Fatalf("last request snapshot should match normalized request")
	}
}

func TestIsResponsesWebsocketPrewarmRequest(t *testing.T) {
	if !isResponsesWebsocketPrewarmRequest([]byte(`{"type":"response.create","model":"test-model","generate":false,"input":[]}`)) {
		t.Fatalf("expected generate=false create request to be treated as prewarm")
	}
	if isResponsesWebsocketPrewarmRequest([]byte(`{"type":"response.create","model":"test-model","generate":true,"input":[]}`)) {
		t.Fatalf("generate=true must not be treated as prewarm")
	}
	if isResponsesWebsocketPrewarmRequest([]byte(`{"type":"response.create","model":"test-model","generate":false,"previous_response_id":"resp-1","input":[]}`)) {
		t.Fatalf("request with previous_response_id must not be treated as prewarm")
	}
}

func TestBuildResponsesWebsocketPrewarmPayloads(t *testing.T) {
	requestJSON := []byte(`{"model":"test-model","instructions":"be helpful","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	_, payloads := buildResponsesWebsocketPrewarmPayloads(requestJSON, false)
	if len(payloads) != 2 {
		t.Fatalf("payload count = %d, want 2", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "type").String() != wsEventTypeCreated {
		t.Fatalf("first payload type = %s, want %s", gjson.GetBytes(payloads[0], "type").String(), wsEventTypeCreated)
	}
	if gjson.GetBytes(payloads[1], "type").String() != wsEventTypeDone {
		t.Fatalf("second payload type = %s, want %s", gjson.GetBytes(payloads[1], "type").String(), wsEventTypeDone)
	}
	createdID := gjson.GetBytes(payloads[0], "response.id").String()
	if createdID == "" {
		t.Fatalf("response.created id must not be empty")
	}
	if !strings.HasPrefix(createdID, "resp_prewarm_") {
		t.Fatalf("unexpected prewarm response id: %s", createdID)
	}
	if gjson.GetBytes(payloads[1], "response.id").String() != createdID {
		t.Fatalf("response.done id must match response.created id")
	}
	if gjson.GetBytes(payloads[1], "response.status").String() != "completed" {
		t.Fatalf("response.done status = %s, want completed", gjson.GetBytes(payloads[1], "response.status").String())
	}
	if gjson.GetBytes(payloads[1], "response.error").Type != gjson.Null {
		t.Fatalf("response.done error must be null")
	}
	if len(gjson.GetBytes(payloads[1], "response.output").Array()) != 0 {
		t.Fatalf("response.done output must be empty")
	}
	if gjson.GetBytes(payloads[1], "response.usage.total_tokens").Int() != 0 {
		t.Fatalf("response.done usage.total_tokens must be zero")
	}
	if gjson.GetBytes(payloads[1], "response.usage.output_tokens_details.reasoning_tokens").Int() != 0 {
		t.Fatalf("response.done usage.output_tokens_details.reasoning_tokens must be zero")
	}
	if gjson.GetBytes(payloads[0], "response.model").String() != "test-model" {
		t.Fatalf("response.created model = %s, want test-model", gjson.GetBytes(payloads[0], "response.model").String())
	}
}

func TestBuildResponsesWebsocketPrewarmPayloadsForCodexV2(t *testing.T) {
	requestJSON := []byte(`{"model":"test-model","stream":true,"input":[]}`)
	_, payloads := buildResponsesWebsocketPrewarmPayloads(requestJSON, true)
	if len(payloads) != 2 {
		t.Fatalf("payload count = %d, want 2", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "type").String() != wsEventTypeCreated {
		t.Fatalf("first payload type = %s, want %s", gjson.GetBytes(payloads[0], "type").String(), wsEventTypeCreated)
	}
	if gjson.GetBytes(payloads[1], "type").String() != wsEventTypeCompleted {
		t.Fatalf("second payload type = %s, want %s", gjson.GetBytes(payloads[1], "type").String(), wsEventTypeCompleted)
	}
}

func TestNormalizeResponsesWebsocketRequestPrewarmThenIncremental(t *testing.T) {
	prewarmRaw := []byte(`{"type":"response.create","model":"test-model","instructions":"be helpful","generate":false,"input":[{"type":"message","id":"msg-1"}]}`)
	_, lastRequest, errMsg := normalizeResponsesWebsocketRequest(prewarmRaw, nil, nil)
	if errMsg != nil {
		t.Fatalf("unexpected prewarm error: %v", errMsg.Error)
	}

	nextRaw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)
	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(nextRaw, lastRequest, []byte("[]"), true, "resp-1", nil)
	if errMsg != nil {
		t.Fatalf("unexpected incremental error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must be removed after local prewarm")
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 1 || input[0].Get("id").String() != "tool-out-1" {
		t.Fatalf("incremental input must remain incremental after prewarm")
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("model should be inherited from prewarm snapshot")
	}
	if gjson.GetBytes(normalized, "instructions").String() != "be helpful" {
		t.Fatalf("instructions should be inherited from prewarm snapshot")
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestPrewarmThenMergedWhenIncrementalDisabled(t *testing.T) {
	prewarmRaw := []byte(`{"type":"response.create","model":"test-model","instructions":"be helpful","generate":false,"input":[{"type":"message","id":"msg-1"}]}`)
	_, lastRequest, errMsg := normalizeResponsesWebsocketRequest(prewarmRaw, nil, nil)
	if errMsg != nil {
		t.Fatalf("unexpected prewarm error: %v", errMsg.Error)
	}

	nextRaw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)
	normalized, _, errMsg := normalizeResponsesWebsocketRequestWithMode(nextRaw, lastRequest, []byte("[]"), false, "", nil)
	if errMsg != nil {
		t.Fatalf("unexpected merged error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must be removed when incremental mode is disabled")
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 2 {
		t.Fatalf("merged input len = %d, want 2", len(input))
	}
	if input[0].Get("id").String() != "msg-1" || input[1].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected merged input order after prewarm fallback")
	}
}

func TestNormalizeResponsesWebsocketRequestCreateWithHistory(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "type").Exists() {
		t.Fatalf("normalized subsequent create request must not include type field")
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("unexpected model: %s", gjson.GetBytes(normalized, "model").String())
	}

	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 4 {
		t.Fatalf("merged input len = %d, want 4", len(input))
	}
	if input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "fc-1" ||
		input[2].Get("id").String() != "assistant-1" ||
		input[3].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected merged input order")
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDIncremental(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true, "", nil)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "type").Exists() {
		t.Fatalf("normalized request must not include type field")
	}
	if gjson.GetBytes(normalized, "previous_response_id").String() != "resp-1" {
		t.Fatalf("previous_response_id must be preserved in incremental mode")
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 1 {
		t.Fatalf("incremental input len = %d, want 1", len(input))
	}
	if input[0].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected incremental input item id: %s", input[0].Get("id").String())
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("unexpected model: %s", gjson.GetBytes(normalized, "model").String())
	}
	if gjson.GetBytes(normalized, "instructions").String() != "be helpful" {
		t.Fatalf("unexpected instructions: %s", gjson.GetBytes(normalized, "instructions").String())
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDMergedWhenIncrementalDisabled(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, false, "", nil)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must be removed when incremental mode is disabled")
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 4 {
		t.Fatalf("merged input len = %d, want 4", len(input))
	}
	if input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "fc-1" ||
		input[2].Get("id").String() != "assistant-1" ||
		input[3].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected merged input order")
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestAppend(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1"},
		{"type":"function_call_output","id":"tool-out-1"}
	]`)
	raw := []byte(`{"type":"response.append","input":[{"type":"message","id":"msg-2"},{"type":"message","id":"msg-3"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 5 {
		t.Fatalf("merged input len = %d, want 5", len(input))
	}
	if input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "assistant-1" ||
		input[2].Get("id").String() != "tool-out-1" ||
		input[3].Get("id").String() != "msg-2" ||
		input[4].Get("id").String() != "msg-3" {
		t.Fatalf("unexpected merged input order")
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized append request")
	}
}

func TestNormalizeResponsesWebsocketRequestAppendWithoutCreate(t *testing.T) {
	raw := []byte(`{"type":"response.append","input":[]}`)

	_, _, errMsg := normalizeResponsesWebsocketRequest(raw, nil, nil)
	if errMsg == nil {
		t.Fatalf("expected error for append without previous request")
	}
	if errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", errMsg.StatusCode, http.StatusBadRequest)
	}
}

func TestWebsocketJSONPayloadsFromChunk(t *testing.T) {
	chunk := []byte("event: response.created\n\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\ndata: [DONE]\n")

	payloads := websocketJSONPayloadsFromChunk(chunk)
	if len(payloads) != 1 {
		t.Fatalf("payloads len = %d, want 1", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "type").String() != "response.created" {
		t.Fatalf("unexpected payload type: %s", gjson.GetBytes(payloads[0], "type").String())
	}
}

func TestWebsocketJSONPayloadsFromPlainJSONChunk(t *testing.T) {
	chunk := []byte(`{"type":"response.completed","response":{"id":"resp-1"}}`)

	payloads := websocketJSONPayloadsFromChunk(chunk)
	if len(payloads) != 1 {
		t.Fatalf("payloads len = %d, want 1", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "type").String() != "response.completed" {
		t.Fatalf("unexpected payload type: %s", gjson.GetBytes(payloads[0], "type").String())
	}
}

func TestResponseCompletedOutputFromPayload(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"message","id":"out-1"}]}}`)

	output := responseCompletedOutputFromPayload(payload)
	items := gjson.ParseBytes(output).Array()
	if len(items) != 1 {
		t.Fatalf("output len = %d, want 1", len(items))
	}
	if items[0].Get("id").String() != "out-1" {
		t.Fatalf("unexpected output id: %s", items[0].Get("id").String())
	}
}

func TestAppendWebsocketEvent(t *testing.T) {
	var builder strings.Builder

	appendWebsocketEvent(&builder, "request", []byte("  {\"type\":\"response.create\"}\n"))
	appendWebsocketEvent(&builder, "response", []byte("{\"type\":\"response.created\"}"))

	got := builder.String()
	if !strings.Contains(got, "websocket.request\n{\"type\":\"response.create\"}\n") {
		t.Fatalf("request event not found in body: %s", got)
	}
	if !strings.Contains(got, "websocket.response\n{\"type\":\"response.created\"}\n") {
		t.Fatalf("response event not found in body: %s", got)
	}
}


func TestAppendWebsocketEventTruncatesAtLimit(t *testing.T) {
	var builder strings.Builder
	payload := bytes.Repeat([]byte("x"), wsBodyLogMaxSize)

	appendWebsocketEvent(&builder, "request", payload)

	got := builder.String()
	if len(got) > wsBodyLogMaxSize {
		t.Fatalf("body log len = %d, want <= %d", len(got), wsBodyLogMaxSize)
	}
	if !strings.Contains(got, wsBodyLogTruncated) {
		t.Fatalf("expected truncation marker in body log")
	}
}

func TestAppendWebsocketEventNoGrowthAfterLimit(t *testing.T) {
	var builder strings.Builder
	appendWebsocketEvent(&builder, "request", bytes.Repeat([]byte("x"), wsBodyLogMaxSize))
	initial := builder.String()

	appendWebsocketEvent(&builder, "response", []byte(`{"type":"response.completed"}`))

	if builder.String() != initial {
		t.Fatalf("builder grew after reaching limit")
	}
}

func TestSetWebsocketRequestBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	setWebsocketRequestBody(c, " \n ")
	if _, exists := c.Get(wsRequestBodyKey); exists {
		t.Fatalf("request body key should not be set for empty body")
	}

	setWebsocketRequestBody(c, "event body")
	value, exists := c.Get(wsRequestBodyKey)
	if !exists {
		t.Fatalf("request body key not set")
	}
	bodyBytes, ok := value.([]byte)
	if !ok {
		t.Fatalf("request body key type mismatch")
	}
	if string(bodyBytes) != "event body" {
		t.Fatalf("request body = %q, want %q", string(bodyBytes), "event body")
	}
}

func TestForwardResponsesWebsocketPreservesCompletedEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	serverErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() {
			errClose := conn.Close()
			if errClose != nil {
				serverErrCh <- errClose
			}
		}()

		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r

		data := make(chan []byte, 1)
		errCh := make(chan *interfaces.ErrorMessage)
		data <- []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[{\"type\":\"message\",\"id\":\"out-1\"}]}}\n\n")
		close(data)
		close(errCh)

		var bodyLog strings.Builder
		result, err := (*OpenAIResponsesAPIHandler)(nil).forwardResponsesWebsocket(
			ctx,
			conn,
			func(...interface{}) {},
			data,
			errCh,
			&bodyLog,
			"session-1",
			true,
			nil,
		)
		if err != nil {
			serverErrCh <- err
			return
		}
		if gjson.GetBytes(result.completedOutput, "0.id").String() != "out-1" {
			serverErrCh <- errors.New("completed output not captured")
			return
		}
		if result.completedResponseID != "resp-1" {
			serverErrCh <- errors.New("completed response id not captured")
			return
		}
		serverErrCh <- nil
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		errClose := conn.Close()
		if errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	_, payload, errReadMessage := conn.ReadMessage()
	if errReadMessage != nil {
		t.Fatalf("read websocket message: %v", errReadMessage)
	}
	if gjson.GetBytes(payload, "type").String() != wsEventTypeCompleted {
		t.Fatalf("payload type = %s, want %s", gjson.GetBytes(payload, "type").String(), wsEventTypeCompleted)
	}
	if strings.Contains(string(payload), "response.done") {
		t.Fatalf("payload unexpectedly rewrote completed event: %s", payload)
	}

	if errServer := <-serverErrCh; errServer != nil {
		t.Fatalf("server error: %v", errServer)
	}
}

func TestResponsesWebsocketCodexV2UsesCompletedEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "auth-ws-codex-v2", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := strings.Replace(server.URL, "http://", "ws://", 1) + "/v1/responses"
	headers := http.Header{}
	headers.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","generate":false,"input":[]}`)); err != nil {
		t.Fatalf("write prewarm: %v", err)
	}

	_, createdPayload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read created payload: %v", err)
	}
	if gjson.GetBytes(createdPayload, "type").String() != wsEventTypeCreated {
		t.Fatalf("created type = %q, want %q", gjson.GetBytes(createdPayload, "type").String(), wsEventTypeCreated)
	}
	prewarmID := gjson.GetBytes(createdPayload, "response.id").String()
	if prewarmID == "" {
		t.Fatalf("prewarm response id is empty")
	}

	_, completedPayload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read completed payload: %v", err)
	}
	if gjson.GetBytes(completedPayload, "type").String() != wsEventTypeCompleted {
		t.Fatalf("completed type = %q, want %q", gjson.GetBytes(completedPayload, "type").String(), wsEventTypeCompleted)
	}
	if gjson.GetBytes(completedPayload, "response.id").String() != prewarmID {
		t.Fatalf("completed response id = %q, want %q", gjson.GetBytes(completedPayload, "response.id").String(), prewarmID)
	}
	if calls, _ := executor.snapshot(); calls != 0 {
		t.Fatalf("executor calls after prewarm = %d, want 0", calls)
	}

	secondRequest := `{"type":"response.create","previous_response_id":"` + prewarmID + `","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(secondRequest)); err != nil {
		t.Fatalf("write second request: %v", err)
	}

	_, secondPayload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read second payload: %v", err)
	}
	if gjson.GetBytes(secondPayload, "type").String() != wsEventTypeCompleted {
		t.Fatalf("second type = %q, want %q", gjson.GetBytes(secondPayload, "type").String(), wsEventTypeCompleted)
	}

	calls, forwardedPayload := executor.snapshot()
	if calls != 1 {
		t.Fatalf("executor calls after second request = %d, want 1", calls)
	}
	if gjson.GetBytes(forwardedPayload, "previous_response_id").Exists() {
		t.Fatalf("forwarded previous_response_id must be removed after local prewarm")
	}
}

func TestResponsesWebsocketPrewarmHandledLocallyAndEnablesIncrementalMode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "auth-ws-prewarm", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := strings.Replace(server.URL, "http://", "ws://", 1) + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","generate":false,"input":[]}`)); err != nil {
		t.Fatalf("write prewarm: %v", err)
	}

	_, createdPayload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read created payload: %v", err)
	}
	if gjson.GetBytes(createdPayload, "type").String() != wsEventTypeCreated {
		t.Fatalf("created type = %q, want %q", gjson.GetBytes(createdPayload, "type").String(), wsEventTypeCreated)
	}
	prewarmID := gjson.GetBytes(createdPayload, "response.id").String()
	if prewarmID == "" {
		t.Fatalf("prewarm response id is empty")
	}

	_, donePayload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read done payload: %v", err)
	}
	if gjson.GetBytes(donePayload, "type").String() != wsEventTypeDone {
		t.Fatalf("done type = %q, want %q", gjson.GetBytes(donePayload, "type").String(), wsEventTypeDone)
	}
	if gjson.GetBytes(donePayload, "response.id").String() != prewarmID {
		t.Fatalf("done response id = %q, want %q", gjson.GetBytes(donePayload, "response.id").String(), prewarmID)
	}
	if calls, _ := executor.snapshot(); calls != 0 {
		t.Fatalf("executor calls after prewarm = %d, want 0", calls)
	}

	secondRequest := `{"type":"response.create","previous_response_id":"` + prewarmID + `","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(secondRequest)); err != nil {
		t.Fatalf("write second request: %v", err)
	}

	_, secondPayload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read second payload: %v", err)
	}
	if gjson.GetBytes(secondPayload, "type").String() != wsEventTypeDone {
		t.Fatalf("second type = %q, want %q", gjson.GetBytes(secondPayload, "type").String(), wsEventTypeDone)
	}

	calls, forwardedPayload := executor.snapshot()
	if calls != 1 {
		t.Fatalf("executor calls after second request = %d, want 1", calls)
	}
	if gjson.GetBytes(forwardedPayload, "previous_response_id").Exists() {
		t.Fatalf("forwarded previous_response_id must be removed after local prewarm")
	}
	if gjson.GetBytes(forwardedPayload, "model").String() != "test-model" {
		t.Fatalf("forwarded model = %q, want %q", gjson.GetBytes(forwardedPayload, "model").String(), "test-model")
	}
	if !gjson.GetBytes(forwardedPayload, "stream").Bool() {
		t.Fatalf("forwarded request must keep stream=true")
	}
	if _, errParse := url.Parse(wsURL); errParse != nil {
		t.Fatalf("unexpected wsURL parse failure: %v", errParse)
	}
}

func TestRestoreResponsesWebsocketChainStateForLocalPrewarm(t *testing.T) {
	responseID := "resp-local-prewarm-test"
	requestJSON := []byte(`{"model":"test-model","instructions":"be helpful","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	if !storeResponsesWebsocketChainState(responseID, "", requestJSON, nil, true) {
		t.Fatalf("expected local prewarm chain state to be stored")
	}

	raw := []byte(`{"type":"response.create","previous_response_id":"resp-local-prewarm-test","input":[{"type":"message","id":"msg-2"}]}`)
	var pinnedAuthID string
	var lastRequest []byte
	lastResponseOutput := []byte{}
	localPrewarmResponseID := ""
	if !restoreResponsesWebsocketChainState(raw, &pinnedAuthID, &lastRequest, &lastResponseOutput, &localPrewarmResponseID) {
		t.Fatalf("expected chain state restore")
	}
	if localPrewarmResponseID != responseID {
		t.Fatalf("localPrewarmResponseID = %q, want %q", localPrewarmResponseID, responseID)
	}

	normalized, _, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true, localPrewarmResponseID, nil)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must be removed for restored local prewarm")
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("model = %q, want test-model", gjson.GetBytes(normalized, "model").String())
	}
	if gjson.GetBytes(normalized, "instructions").String() != "be helpful" {
		t.Fatalf("instructions = %q, want be helpful", gjson.GetBytes(normalized, "instructions").String())
	}
}

func TestResponsesWebsocketPreviousResponseNotFoundFallsBackToMergedInput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &websocketRetryExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{
		ID:       "auth-ws-retry",
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"websockets": true},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := strings.Replace(server.URL, "http://", "ws://", 1) + "/v1/responses"

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	firstRequest := `{"type":"response.create","model":"test-model","instructions":"be helpful","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}]}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(firstRequest)); err != nil {
		t.Fatalf("write first request: %v", err)
	}
	_, firstPayload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read first payload: %v", err)
	}
	if got := gjson.GetBytes(firstPayload, "response.id").String(); got != "resp-1" {
		t.Fatalf("first response id = %q, want resp-1", got)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close first websocket: %v", err)
	}

	conn, _, err = websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial second websocket: %v", err)
	}
	defer conn.Close()

	secondRequest := `{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"follow"}]}]}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(secondRequest)); err != nil {
		t.Fatalf("write second request: %v", err)
	}

	_, secondPayload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read second payload: %v", err)
	}
	secondType := gjson.GetBytes(secondPayload, "type").String()
	if secondType != wsEventTypeDone && secondType != wsEventTypeCompleted {
		t.Fatalf("second payload type = %q, want response.done/completed", secondType)
	}
	if got := gjson.GetBytes(secondPayload, "response.id").String(); got != "resp-2" {
		t.Fatalf("second response id = %q, want resp-2", got)
	}
	if gjson.GetBytes(secondPayload, "error").Exists() {
		t.Fatalf("unexpected error payload: %s", secondPayload)
	}

	payloads := executor.snapshot()
	if len(payloads) != 3 {
		t.Fatalf("executor calls = %d, want 3", len(payloads))
	}
	if gjson.GetBytes(payloads[1], "previous_response_id").String() != "resp-1" {
		t.Fatalf("second upstream payload previous_response_id = %q, want resp-1", gjson.GetBytes(payloads[1], "previous_response_id").String())
	}
	if gjson.GetBytes(payloads[2], "previous_response_id").Exists() {
		t.Fatalf("fallback payload must remove previous_response_id")
	}
	if gjson.GetBytes(payloads[2], "model").String() != "test-model" {
		t.Fatalf("fallback model = %q, want test-model", gjson.GetBytes(payloads[2], "model").String())
	}
	if gjson.GetBytes(payloads[2], "instructions").String() != "be helpful" {
		t.Fatalf("fallback instructions = %q, want be helpful", gjson.GetBytes(payloads[2], "instructions").String())
	}
	if len(gjson.GetBytes(payloads[2], "input").Array()) != 3 {
		t.Fatalf("fallback input length = %d, want 3", len(gjson.GetBytes(payloads[2], "input").Array()))
	}
}
