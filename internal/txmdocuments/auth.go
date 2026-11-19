package txmdocuments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
)

const maxIdentityBytes = 1 << 20

type tokenCache struct {
	mu                            sync.Mutex
	client                        *http.Client
	endpoint, resource, tokenFile string
	readFile                      func(string) ([]byte, error)
	now                           func() time.Time
	token                         string
	expiresAt, refreshAt          time.Time
}

func (c *tokenCache) get(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.token != "" && now.Before(c.refreshAt) && now.Before(c.expiresAt) {
		return c.token, nil
	}
	token, life, err := c.exchange(ctx)
	if err != nil {
		if c.token != "" && c.now().Before(c.expiresAt) {
			return c.token, nil
		}
		return "", err
	}
	c.token, c.expiresAt, c.refreshAt = token, now.Add(life), now.Add(life/2)
	if !c.now().Before(c.expiresAt) {
		c.token = ""
		return "", errors.New("document service exchanged credential already expired")
	}
	return c.token, nil
}

func (c *tokenCache) invalidate(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token = ""
		c.refreshAt = time.Time{}
		c.expiresAt = time.Time{}
	}
}

func (c *tokenCache) exchange(ctx context.Context) (string, time.Duration, error) {
	subject, err := c.readFile(c.tokenFile)
	if err != nil {
		return "", 0, errors.New("document service could not read IDENTITY_TOKEN_FILE")
	}
	if len(subject) == 0 || len(subject) > maxIdentityBytes || strings.TrimSpace(string(subject)) == "" {
		return "", 0, errors.New("document service projected identity credential is empty or too large")
	}
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"},
		"subject_token":      {strings.TrimSpace(string(subject))},
		"resource":           {c.resource},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, errors.New("document service identity exchange request is invalid")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return "", 0, errors.New("document service identity exchange failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", 0, errors.New("document service identity exchange was rejected")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxIdentityBytes+1))
	if err != nil || len(body) > maxIdentityBytes {
		return "", 0, errors.New("document service identity exchange response is invalid")
	}
	var result struct {
		AccessToken     string      `json:"access_token"`
		TokenType       string      `json:"token_type"`
		IssuedTokenType string      `json:"issued_token_type"`
		ExpiresIn       json.Number `json:"expires_in"`
	}
	if json.Unmarshal(body, &result) != nil {
		return "", 0, errors.New("document service identity exchange response is invalid")
	}
	seconds, err := result.ExpiresIn.Int64()
	if err != nil || seconds <= 0 || seconds > math.MaxInt64/int64(time.Second) || result.AccessToken == "" ||
		strings.ContainsAny(result.AccessToken, " \t\r\n") ||
		!strings.EqualFold(result.TokenType, "Bearer") ||
		result.IssuedTokenType != "" && result.IssuedTokenType != "urn:ietf:params:oauth:token-type:access_token" {
		return "", 0, errors.New("document service identity exchange response is invalid")
	}
	return result.AccessToken, time.Duration(seconds) * time.Second, nil
}

type authInterceptor struct{ tokens *tokenCache }

func (i authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
		for attempt := 0; attempt < 2; attempt++ {
			token, err := i.tokens.get(ctx)
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, err)
			}
			request.Header().Set("Authorization", "Bearer "+token)
			response, err := next(ctx, request)
			if connect.CodeOf(err) == connect.CodeUnauthenticated {
				i.tokens.invalidate(token)
				if attempt == 0 {
					continue
				}
			}
			return response, err
		}
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("document service authentication failed"))
	}
}

func (i authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
