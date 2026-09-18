package aiprotocol

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// Model list fetching (design.md §12.5). Anthropic's endpoint pages with
// has_more/last_id, so a single page would quietly hide most of the catalogue.

func TestFetchModelsOpenAI(t *testing.T) {
	for _, protocol := range []string{ProtocolOpenAICompletions, ProtocolOpenAIResponses} {
		t.Run(protocol, func(t *testing.T) {
			f := newJSONFixture(t, http.StatusOK, `{"object":"list","data":[
				{"id":"gpt-4o","object":"model"},
				{"id":"gpt-4o-mini","name":"GPT-4o mini"},
				{"id":"gpt-4o"},
				{"id":""}
			]}`)
			req := baseRequest(f, protocol)
			models, err := FetchModels(context.Background(), f.Client(), req)
			if err != nil {
				t.Fatalf("FetchModels: %v", err)
			}

			got := f.last(t)
			if got.Method != http.MethodGet {
				t.Fatalf("method = %q, want GET", got.Method)
			}
			if got.Path != "/v1/models" {
				t.Fatalf("path = %q, want /v1/models", got.Path)
			}
			if auth := got.Header.Get("Authorization"); auth != "Bearer "+testAPIKey {
				t.Fatalf("Authorization = %q", auth)
			}

			want := []FetchedModel{
				{ID: "gpt-4o", DisplayName: "gpt-4o"}, // no name in the body: the id stands in
				{ID: "gpt-4o-mini", DisplayName: "GPT-4o mini"},
			}
			if len(models) != len(want) {
				t.Fatalf("models = %+v, want %+v (empty id dropped, duplicates collapsed)", models, want)
			}
			for i := range want {
				if models[i] != want[i] {
					t.Fatalf("models[%d] = %+v, want %+v", i, models[i], want[i])
				}
			}
		})
	}
}

func TestFetchModelsAnthropicPaginates(t *testing.T) {
	var (
		mu   sync.Mutex
		page int
	)
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		page++
		current := page
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch current {
		case 1:
			_, _ = w.Write([]byte(`{"data":[
				{"id":"claude-3-5-sonnet","display_name":"Claude 3.5 Sonnet","type":"model"},
				{"id":"claude-3-haiku","display_name":"Claude 3 Haiku","type":"model"}
			],"has_more":true,"first_id":"claude-3-5-sonnet","last_id":"claude-3-haiku"}`))
		default:
			_, _ = w.Write([]byte(`{"data":[
				{"id":"claude-3-opus","display_name":"Claude 3 Opus","type":"model"}
			],"has_more":false,"first_id":"claude-3-opus","last_id":"claude-3-opus"}`))
		}
	})

	req := baseRequest(f, ProtocolAnthropicMessages)
	models, err := FetchModels(context.Background(), f.Client(), req)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	got := f.requests()
	if len(got) != 2 {
		t.Fatalf("made %d requests, want 2 pages", len(got))
	}
	if got[0].Path != "/v1/models" {
		t.Fatalf("path = %q", got[0].Path)
	}
	if v := got[0].Query.Get("limit"); v != "1000" {
		t.Fatalf("first page limit = %q, want 1000", v)
	}
	if _, ok := got[0].Query["after_id"]; ok {
		t.Fatalf("the first page must not carry a cursor: %v", got[0].Query)
	}
	// The cursor is the previous page's last_id — not first_id, and not an
	// offset.
	if v := got[1].Query.Get("after_id"); v != "claude-3-haiku" {
		t.Fatalf("second page after_id = %q, want claude-3-haiku", v)
	}
	if v := got[1].Header.Get("x-api-key"); v != testAPIKey {
		t.Fatalf("x-api-key = %q", v)
	}
	if v := got[1].Header.Get("anthropic-version"); v != anthropicVersion {
		t.Fatalf("anthropic-version = %q", v)
	}

	want := []FetchedModel{
		{ID: "claude-3-5-sonnet", DisplayName: "Claude 3.5 Sonnet"},
		{ID: "claude-3-haiku", DisplayName: "Claude 3 Haiku"},
		{ID: "claude-3-opus", DisplayName: "Claude 3 Opus"},
	}
	if len(models) != len(want) {
		t.Fatalf("models = %+v, want %+v", models, want)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Fatalf("models[%d] = %+v, want %+v", i, models[i], want[i])
		}
	}
}

// TestFetchModelsAnthropicStuckCursor: has_more=true with no usable cursor would
// loop forever inside a request handler.
func TestFetchModelsAnthropicStuckCursor(t *testing.T) {
	f := newJSONFixture(t, http.StatusOK, `{"data":[{"id":"a"}],"has_more":true,"last_id":""}`)
	_, err := FetchModels(context.Background(), f.Client(), baseRequest(f, ProtocolAnthropicMessages))
	if err == nil || !strings.Contains(err.Error(), "has_more") {
		t.Fatalf("want a cursor error, got %v", err)
	}
	if got := len(f.requests()); got != 1 {
		t.Fatalf("made %d requests, want 1 (no infinite walk)", got)
	}
}

// TestFetchModelsEmptyIsNotAnError: a provider with no models is a valid (if
// useless) answer; the panel shows an empty list.
func TestFetchModelsEmptyIsNotAnError(t *testing.T) {
	f := newJSONFixture(t, http.StatusOK, `{"data":[]}`)
	models, err := FetchModels(context.Background(), f.Client(), baseRequest(f, ProtocolAnthropicMessages))
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if models == nil {
		t.Fatal("want an empty slice, not nil (JSON null would surprise the panel)")
	}
	if len(models) != 0 {
		t.Fatalf("models = %+v", models)
	}
}

// TestFetchModelsHTMLAnswer: a wrong base_url often answers 200 with an HTML
// page — that has to be an error, not an empty catalogue.
func TestFetchModelsHTMLAnswer(t *testing.T) {
	f := newJSONFixture(t, http.StatusOK, `<html><body>not an API</body></html>`)
	_, err := FetchModels(context.Background(), f.Client(), baseRequest(f, ProtocolOpenAICompletions))
	if err == nil {
		t.Fatal("want a decode error")
	}
}
