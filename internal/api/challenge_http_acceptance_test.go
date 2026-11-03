package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestChallengeRealHTTPFrozenAcceptance(t *testing.T) {
	h := newHarness(t, nil)
	_, questionSecret := h.questionSite(t, "frozen-http-question", "pk-frozen-question", 3)
	h.fillPool(t, 3, 8)
	imageSite, imageSecret := h.imageSiteWithSecret(t, "frozen-http-image", "pk-frozen-image", 3)
	image := imageFixture(t, 3, "frozen-http-acceptance")
	h.seedQuestions(t, imageSite.Policy.Difficulty, image)
	var clockMu sync.RWMutex
	h.srv.now = func() time.Time {
		clockMu.RLock()
		defer clockMu.RUnlock()
		return h.clock
	}
	server := httptest.NewServer(h.handler())
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}

	request := func(path string, payload any, headers ...map[string]string) reply {
		t.Helper()
		var body io.Reader
		contentType := "application/json"
		if form, ok := payload.(url.Values); ok {
			body, contentType = strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"
		} else {
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(raw)
		}
		req, err := http.NewRequest(http.MethodPost, server.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", contentType)
		for _, values := range headers {
			for key, value := range values {
				req.Header.Set(key, value)
			}
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(res.Body, 128<<10))
		if err != nil {
			t.Fatal(err)
		}
		r := reply{status: res.StatusCode, body: string(raw), headers: res.Header.Clone()}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&r.got); err != nil {
			t.Fatalf("response was not JSON: status=%d", res.StatusCode)
		}
		return r
	}
	solve := func(r reply) string {
		t.Helper()
		pow, ok := r.got["pow"].(map[string]any)
		if !ok {
			t.Fatal("successful challenge had no PoW")
		}
		seed, err := base64.RawURLEncoding.DecodeString(pow["seed"].(string))
		if err != nil {
			t.Fatal(err)
		}
		bits := int(num(pow["bits"]))
		for n := 0; n < 2_000_000; n++ {
			nonce := strconv.Itoa(n)
			hash := sha256.Sum256(append(append([]byte{}, seed...), nonce...))
			zeros := 0
			for _, b := range hash {
				for mask := byte(0x80); mask != 0 && b&mask == 0; mask >>= 1 {
					zeros++
				}
				if b != 0 {
					break
				}
			}
			if zeros >= bits {
				return nonce
			}
		}
		t.Fatal("independent PoW solver exhausted its bound")
		return ""
	}
	verify := func(secret, token string) reply {
		return request("/siteverify", url.Values{"secret": {secret}, "response": {token}})
	}

	for _, scenario := range []struct {
		name, key, secret, imageAnswer string
		question                       bool
	}{
		{"pow-only", "sk-alpha", sentinelSecret, "", false},
		{"pow-question", "pk-frozen-question", questionSecret, "", true},
		{"image-question", "pk-frozen-image", imageSecret, image.Answer, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			challenge := request("/api/challenge?site_key="+scenario.key, map[string]string{"site_key": scenario.key})
			if challenge.status != http.StatusOK || challenge.str("token") == "" {
				t.Fatalf("challenge HTTP %d", challenge.status)
			}
			payload := map[string]string{"token": challenge.str("token"), "pow_nonce": solve(challenge)}
			if scenario.imageAnswer != "" {
				imageQuestionOf(t, challenge)
				payload["answer"] = scenario.imageAnswer
			} else if scenario.question {
				payload["answer"] = strconv.FormatInt(solveTaskText(t, h.questionOf(t, challenge)), 10)
			}
			redemption := request("/api/redeem?site_key="+scenario.key, payload)
			if redemption.status != http.StatusOK || redemption.str("response") == "" {
				t.Fatalf("redeem HTTP %d", redemption.status)
			}
			if r := request("/api/redeem?site_key="+scenario.key, payload); r.status == http.StatusOK {
				t.Fatal("challenge replay succeeded")
			}
			response := redemption.str("response")
			if r := verify("not-a-valid-site-secret", response); r.hasSuccess() {
				t.Fatal("wrong secret authorized response")
			}
			if r := verify(scenario.secret, response); !r.hasSuccess() {
				t.Fatal("correct secret failed after wrong secret attempt")
			}
			if r := verify(scenario.secret, response); r.hasSuccess() || !r.hasCode(ErrTimeoutOrDuplicate) {
				t.Fatal("response replay was not rejected")
			}
		})
	}

	t.Run("site-conflict-does-not-consume", func(t *testing.T) {
		c := request("/api/challenge", map[string]string{"site_key": "sk-alpha"})
		payload := map[string]string{"token": c.str("token"), "pow_nonce": solve(c)}
		if r := request("/api/redeem?site_key=sk-bravo", payload); r.status != http.StatusBadRequest {
			t.Fatalf("conflicting site got HTTP %d", r.status)
		}
		if r := request("/api/redeem?site_key=sk-alpha", payload); r.status != http.StatusOK {
			t.Fatal("site conflict consumed challenge")
		}
	})

	t.Run("wrong-answer-terminates", func(t *testing.T) {
		c := request("/api/challenge", map[string]string{"site_key": "pk-frozen-question"})
		answer := solveTaskText(t, h.questionOf(t, c))
		payload := map[string]string{"token": c.str("token"), "pow_nonce": solve(c), "answer": strconv.FormatInt(answer+1, 10)}
		if r := request("/api/redeem", payload); r.status == http.StatusOK {
			t.Fatal("wrong answer succeeded")
		}
		payload["answer"] = strconv.FormatInt(answer, 10)
		if r := request("/api/redeem", payload); r.status == http.StatusOK {
			t.Fatal("wrong answer did not terminate challenge")
		}
	})

	t.Run("origin-conflict-does-not-consume", func(t *testing.T) {
		h.setOrigins(h.siteA.ID, []string{"https://app.example.com", "https://other.example.com"})
		origin := map[string]string{"Origin": "https://app.example.com"}
		c := request("/api/challenge", map[string]string{"site_key": "sk-alpha"}, origin)
		if c.status != http.StatusOK {
			t.Fatalf("allowed origin challenge HTTP %d", c.status)
		}
		payload := map[string]string{"token": c.str("token"), "pow_nonce": solve(c)}
		if r := request("/api/redeem", payload, map[string]string{"Origin": "https://other.example.com"}); r.status != http.StatusBadRequest {
			t.Fatalf("changed origin got HTTP %d", r.status)
		}
		r := request("/api/redeem", payload, origin)
		if r.status != http.StatusOK || !verify(sentinelSecret, r.str("response")).hasSuccess() {
			t.Fatal("origin conflict consumed the valid challenge")
		}
	})

	t.Run("pool-empty-recovers-without-downgrade", func(t *testing.T) {
		_, secret := h.questionSite(t, "frozen-empty-pool", "pk-frozen-empty", 5)
		c := request("/api/challenge", map[string]string{"site_key": "pk-frozen-empty"})
		if c.status != http.StatusServiceUnavailable || !c.hasCode(CodeChallengeUnavailable) || c.headers.Get("Retry-After") == "" {
			t.Fatalf("empty pool must return retryable 503, got HTTP %d", c.status)
		}
		if c.got["token"] != nil || c.got["pow"] != nil {
			t.Fatal("empty pool downgraded to PoW-only")
		}
		h.fillPool(t, 5, 2)
		c = request("/api/challenge", map[string]string{"site_key": "pk-frozen-empty"})
		if c.status != http.StatusOK {
			t.Fatalf("refilled pool challenge HTTP %d", c.status)
		}
		payload := map[string]string{
			"token": c.str("token"), "pow_nonce": solve(c),
			"answer": strconv.FormatInt(solveTaskText(t, h.questionOf(t, c)), 10),
		}
		r := request("/api/redeem", payload)
		if r.status != http.StatusOK || !verify(secret, r.str("response")).hasSuccess() {
			t.Fatal("refilled pool did not restore the full flow")
		}
	})

	t.Run("expired-challenge-is-rejected", func(t *testing.T) {
		c := request("/api/challenge", map[string]string{"site_key": "sk-alpha"})
		payload := map[string]string{"token": c.str("token"), "pow_nonce": solve(c)}
		clockMu.Lock()
		h.setClock(2 * time.Minute)
		clockMu.Unlock()
		if r := request("/api/redeem", payload); r.status == http.StatusOK {
			t.Fatal("expired challenge succeeded")
		}
	})
}
