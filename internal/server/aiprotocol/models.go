package aiprotocol

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// FetchModels lists a provider's models with GET {BaseURL}/models (§12.5).
//
// Both OpenAI dialects share one endpoint shape; Anthropic publishes the same
// path but a different body AND cursor pagination, which has to be followed —
// a single page would silently hide most of the catalogue.
//
// A 404 (or an HTML answer from a gateway without the endpoint) is an error
// here; the panel degrades to hand-typed model ids on its side. That decision
// belongs to the caller: this layer reports what the provider said.

// anthropicModelsPageLimit asks for the largest page Anthropic allows, so the
// common case is one round trip.
const anthropicModelsPageLimit = 1000

// anthropicMaxModelPages bounds the cursor walk. A provider that keeps saying
// has_more=true would otherwise loop forever inside a request handler; 50 pages
// is 50k models, far past any real catalogue.
const anthropicMaxModelPages = 50

// FetchModels calls `GET {BaseURL}/models` and returns the entries in the order
// the provider listed them, de-duplicated by id.
func FetchModels(ctx context.Context, client *http.Client, req Request) ([]FetchedModel, error) {
	if client == nil {
		return nil, errors.New("aiprotocol: nil http client (the caller supplies the timeout policy)")
	}
	base, err := endpointURL(req.BaseURL, "")
	if err != nil {
		return nil, err
	}
	switch req.Protocol {
	case ProtocolOpenAICompletions, ProtocolOpenAIResponses:
		return fetchOpenAIModels(ctx, client, req, base)
	case ProtocolAnthropicMessages:
		return fetchAnthropicModels(ctx, client, req, base)
	default:
		return nil, fmt.Errorf(
			"aiprotocol: unknown protocol %q (want %s, %s or %s)",
			req.Protocol, ProtocolOpenAICompletions, ProtocolOpenAIResponses, ProtocolAnthropicMessages,
		)
	}
}

// fetchOpenAIModels reads the {"data":[{"id":…,"name":…}]} list. Gateways
// routinely omit the display name (the official API does), so the id is the
// fallback.
func fetchOpenAIModels(ctx context.Context, client *http.Client, req Request, base string) ([]FetchedModel, error) {
	endpoint := base + "/models"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build models request for %s: %w", endpoint, err)
	}
	if err := applyAuthHeaders(httpReq.Header, req, authBearer); err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Protocol, endpoint, redactError(err, req.APIKey))
	}
	defer resp.Body.Close()

	var payload struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := decodeJSONBody(req.Protocol, endpoint, resp, req.APIKey, &payload); err != nil {
		return nil, err
	}

	out := make([]FetchedModel, 0, len(payload.Data))
	seen := make(map[string]bool, len(payload.Data))
	for _, m := range payload.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = id
		}
		out = append(out, FetchedModel{ID: id, DisplayName: name})
	}
	return out, nil
}

// fetchAnthropicModels follows Anthropic's has_more/last_id cursor.
func fetchAnthropicModels(ctx context.Context, client *http.Client, req Request, base string) ([]FetchedModel, error) {
	out := []FetchedModel{}
	seen := map[string]bool{}
	after := ""

	for page := 0; page < anthropicMaxModelPages; page++ {
		query := url.Values{}
		query.Set("limit", strconv.Itoa(anthropicModelsPageLimit))
		if after != "" {
			// Anthropic's cursor parameter is after_id (the response's last_id).
			query.Set("after_id", after)
		}
		endpoint := base + "/models?" + query.Encode()

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("build models request for %s: %w", endpoint, err)
		}
		if err := applyAuthHeaders(httpReq.Header, req, authAnthropic); err != nil {
			return nil, err
		}
		httpReq.Header.Set("Accept", "application/json")

		resp, err := client.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", req.Protocol, endpoint, redactError(err, req.APIKey))
		}

		var payload struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		err = decodeJSONBody(req.Protocol, endpoint, resp, req.APIKey, &payload)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		for _, m := range payload.Data {
			id := strings.TrimSpace(m.ID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			name := strings.TrimSpace(m.DisplayName)
			if name == "" {
				name = id
			}
			out = append(out, FetchedModel{ID: id, DisplayName: name})
		}

		if !payload.HasMore {
			return out, nil
		}
		next := strings.TrimSpace(payload.LastID)
		if next == "" || next == after {
			// has_more with no usable cursor: following it again would either
			// loop forever or fetch the same page. Report it instead.
			return nil, fmt.Errorf(
				"%s %s: provider says has_more=true but last_id is %q (cannot follow the cursor)",
				req.Protocol, endpoint, payload.LastID,
			)
		}
		after = next
	}
	return nil, fmt.Errorf(
		"%s: model list did not finish after %d pages (has_more never cleared)",
		req.Protocol, anthropicMaxModelPages,
	)
}
