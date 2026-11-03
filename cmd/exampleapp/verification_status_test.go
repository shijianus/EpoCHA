package main

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
)

func TestNonOKVerificationNeverAuthorizesBusinessWrite(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		for _, form := range []bool{false, true} {
			t.Run(strconv.Itoa(status)+"/form="+strconv.FormatBool(form), func(t *testing.T) {
				a, sb := newTestApp(t, func(int, url.Values) (int, any) {
					return status, map[string]any{"success": true}
				})
				var code int
				if form {
					code = postForm(t, a, url.Values{"text": {"must not be committed"}, "epocha_response": {"unexpected-success"}}, nil).Code
				} else {
					code = post(t, a, bodyWith("unexpected-success", "must not be committed"), nil).Code
				}
				if code == http.StatusCreated || countMessages(t, a) != 0 {
					t.Fatalf("upstream status %d authorized a business write (HTTP %d)", status, code)
				}
				if sb.count() != 1 {
					t.Fatalf("verification calls = %d, want 1", sb.count())
				}
			})
		}
	}
}
