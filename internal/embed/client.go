package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	base    string
	key     string
	model   string
	http    *http.Client
	limiter *limiter
}

func New(base, key, model string) *Client {
	return &Client{
		base:    base,
		key:     key,
		model:   model,
		limiter: limiterFromEnv(),
		http: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// RateLimitError is a 429 from the embeddings endpoint. It carries the wait the
// server itself asks for, which is the only one that helps: retrying earlier
// hits the same limit again, and giving up after 7 s loses the chunks of that
// document.
type RateLimitError struct {
	RetryAfter time.Duration
	Body       string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("embed http 429 (wait %s): %s", e.RetryAfter, e.Body)
}

// waitRequestedByServer looks at the standard Retry-After header and, if absent,
// at the retry_after_seconds field of the body.
func waitRequestedByServer(header string, body []byte) time.Duration {
	if header != "" {
		if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		if when, err := http.ParseTime(header); err == nil {
			if wait := time.Until(when); wait > 0 {
				return wait
			}
		}
	}
	var parsed struct {
		RetryAfterSeconds float64 `json:"retry_after_seconds"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.RetryAfterSeconds > 0 {
		return capWait(time.Duration(parsed.RetryAfterSeconds * float64(time.Second)))
	}
	// The upstream limit does not send retry_after_seconds: it says in the
	// message "Limit resets at: 2026-09-14 23:27:45 UTC".
	if m := resetInMessage.FindSubmatch(body); m != nil {
		if when, err := time.ParseInLocation("2006-01-02 15:04:05", string(m[1]), time.UTC); err == nil {
			if wait := time.Until(when); wait > 0 {
				return capWait(wait)
			}
		}
	}
	return 0
}

// resetInMessage pulls the reset time out of the error text.
var resetInMessage = regexp.MustCompile(`Limit resets at: (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) UTC`)

// maxWait is the ceiling of what is waited for a 429. The server clock is not
// ours: an absurd reset time must not leave the pass stopped for half an hour.
const maxWait = 90 * time.Second

func capWait(d time.Duration) time.Duration {
	if d > maxWait {
		return maxWait
	}
	return d
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// Embed sends a batch of inputs and returns just the vectors (in input order).
// For token accounting use EmbedWithUsage.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	vecs, _, err := c.EmbedWithUsage(ctx, inputs)
	return vecs, err
}

// EmbedWithUsage sends a batch of inputs and returns vectors plus the total
// tokens billed for the call (as reported by the API in usage.total_tokens).
func (c *Client) EmbedWithUsage(ctx context.Context, inputs []string) ([][]float32, int, error) {
	if len(inputs) == 0 {
		return nil, 0, nil
	}
	body, err := json.Marshal(embedRequest{Model: c.model, Input: inputs})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	req.Header.Set("Content-Type", "application/json")

	// Ask for a turn before leaving: when a rate limit is configured, hitting it
	// costs a wait per collision.
	c.limiter.waitTurn(ctx)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, 0, &RateLimitError{
				RetryAfter: waitRequestedByServer(resp.Header.Get("Retry-After"), b),
				Body:       string(b),
			}
		}
		return nil, 0, fmt.Errorf("embed http %d: %s", resp.StatusCode, string(b))
	}

	var er embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, 0, err
	}
	if len(er.Data) != len(inputs) {
		return nil, 0, fmt.Errorf("embed: expected %d vectors, got %d", len(inputs), len(er.Data))
	}
	out := make([][]float32, len(inputs))
	for _, d := range er.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, 0, fmt.Errorf("embed: out-of-range index %d", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	return out, er.Usage.TotalTokens, nil
}
