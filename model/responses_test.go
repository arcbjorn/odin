package model

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func sseResponse(status int, body string) *http.Response {
	resp := jsonResponse(status, body)
	resp.Header.Set("Content-Type", "text/event-stream")
	return resp
}

func TestResponsesCodexRequestAndToolCall(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-test"},
	})
	token := "x." + base64.RawURLEncoding.EncodeToString(claims) + ".y"

	provider := NewResponses(ResponsesConfig{
		Provider: "codex", Model: "gpt-5.5", BaseURL: "https://chatgpt.test/backend-api/codex",
		Tokens: StaticToken(token), Codex: true,
	})
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("path = %s", req.URL.Path)
		}
		if got := req.Header.Get("originator"); got != "codex_cli_rs" {
			t.Fatalf("originator = %q", got)
		}
		if got := req.Header.Get("ChatGPT-Account-ID"); got != "acct-test" {
			t.Fatalf("account header = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["instructions"] != "stable system" || body["store"] != false {
			t.Fatalf("body = %#v", body)
		}
		// Every function tool must carry a strict field. Without it the codex
		// backend silently ignores the tool and the model never calls it.
		sentTools, ok := body["tools"].([]any)
		if !ok || len(sentTools) != 1 {
			t.Fatalf("tools = %#v", body["tools"])
		}
		toolObj := sentTools[0].(map[string]any)
		if _, present := toolObj["strict"]; !present {
			t.Fatalf("function tool must include strict, got %#v", toolObj)
		}
		if toolObj["parameters"] == nil {
			t.Fatalf("function tool must include parameters, got %#v", toolObj)
		}
		// codex rejects non-streaming requests with 400, so the transport must
		// set stream and ask for SSE.
		if body["stream"] != true {
			t.Fatalf("codex request must set stream=true, got %#v", body["stream"])
		}
		// The codex proxy rejects max_output_tokens with a 400.
		if _, present := body["max_output_tokens"]; present {
			t.Fatalf("codex request must omit max_output_tokens, got %#v", body["max_output_tokens"])
		}
		if got := req.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("Accept = %q, want text/event-stream", got)
		}
		// A realistic codex stream: an in-progress event, then the terminal
		// completed event carrying the response object.
		sse := "event: response.in_progress\n" +
			`data: {"type":"response.in_progress","response":{"status":"in_progress","output":[]}}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"model":"gpt-5.5","status":"completed",` +
			`"output":[{"type":"function_call","call_id":"call-1","name":"query","arguments":"{\"sql\":\"select 1\"}"}],` +
			`"usage":{"input_tokens":12,"output_tokens":4,"input_tokens_details":{"cached_tokens":8}}}}` + "\n\n" +
			"data: [DONE]\n\n"
		return sseResponse(http.StatusOK, sse), nil
	})}

	response, err := provider.Complete(context.Background(), Request{
		System: "stable system", Messages: []Message{{Role: RoleUser, Content: "go"}},
		Tools: []Tool{{Name: "query", Schema: map[string]any{"type": "object"}}},
		Effort: "high", MaxTokens: 4096, // must be dropped for codex
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != StopToolUse || len(response.ToolCalls) != 1 || response.ToolCalls[0].Name != "query" {
		t.Fatalf("response = %+v", response)
	}
	if response.Usage.Cached != 8 {
		t.Fatalf("usage = %+v", response.Usage)
	}
}

func TestFinalResponseFromSSE(t *testing.T) {
	// The terminal completed event's response object is what downstream
	// parsing expects — later events win over earlier snapshots.
	stream := "data: {\"type\":\"response.in_progress\",\"response\":{\"status\":\"in_progress\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-5.5\"}}\n\n" +
		"data: [DONE]\n\n"
	out, err := finalResponseFromSSE([]byte(stream))
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	var got struct{ Status, Model string }
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal reduced response: %v", err)
	}
	if got.Status != "completed" || got.Model != "gpt-5.5" {
		t.Fatalf("reduced to %+v", got)
	}
}

func TestFinalResponseFromSSEWithoutTerminalEventErrors(t *testing.T) {
	// A stream that never completes must not silently yield an empty response.
	stream := "data: {\"type\":\"response.in_progress\",\"response\":{\"status\":\"in_progress\"}}\n\n"
	if _, err := finalResponseFromSSE([]byte(stream)); err == nil {
		t.Fatal("expected an error when no terminal event is present")
	}
}

func TestResponsesReplaysOpaqueOutput(t *testing.T) {
	provider := NewResponses(ResponsesConfig{
		Provider: "codex", Model: "gpt-5.6-sol", BaseURL: "https://chatgpt.test/backend-api/codex",
		Tokens: StaticToken("token"), Codex: true,
	})
	call := 0
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		call++
		var body struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if call == 2 {
			joinedBytes, err := json.Marshal(body.Input)
			if err != nil {
				t.Fatal(err)
			}
			joined := string(joinedBytes)
			if !strings.Contains(joined, `"type":"reasoning"`) || !strings.Contains(joined, `"encrypted_content":"opaque"`) {
				t.Fatalf("opaque response output was not replayed: %s", joined)
			}
			return sseResponse(http.StatusOK, "data: {\"type\":\"response.completed\",\"response\":"+
				`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}}`+"\n\n"), nil
		}
		return sseResponse(http.StatusOK, "data: {\"type\":\"response.completed\",\"response\":"+
			`{"status":"completed","output":[{"type":"reasoning","encrypted_content":"opaque"},{"type":"function_call","call_id":"c1","name":"query","arguments":"{}"}]}}`+"\n\n"), nil
	})}

	first, err := provider.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if first.ProviderState == nil {
		t.Fatal("missing provider state")
	}
	_, err = provider.Complete(context.Background(), Request{Messages: []Message{
		{Role: RoleUser, Content: "go"},
		{Role: RoleAssistant, ToolCalls: first.ToolCalls, ProviderState: first.ProviderState},
		{Role: RoleTool, ToolCallID: "c1", Content: "ok"},
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestXAIResponsesOmitsReasoningEffort(t *testing.T) {
	provider := NewResponses(ResponsesConfig{
		Provider: "xai", Model: "grok-4.20", BaseURL: "https://api.x.ai/v1",
		Tokens: StaticToken("token"), XAI: true,
	})
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, exists := body["reasoning"]; exists {
			t.Fatalf("xAI request must omit unsupported reasoning effort: %#v", body)
		}
		return jsonResponse(http.StatusOK, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`), nil
	})}

	response, err := provider.Complete(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hello"}}, Effort: "high",
	})
	if err != nil || response.Text != "ok" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}
