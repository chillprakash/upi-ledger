package psp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chillprakash/upi-ledger/internal/upitest"
)

func TestHTTP(t *testing.T) {
	w := upitest.New(t)
	srv := httptest.NewServer(w.App.Handler())
	defer srv.Close()
	post := func(key, body string) (*http.Response, map[string]string) {
		req, _ := http.NewRequest("POST", srv.URL+"/payments", strings.NewReader(body))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		m := map[string]string{}
		json.NewDecoder(res.Body).Decode(&m) //nolint:errcheck
		return res, m
	}
	body := `{"payer":"asha@hdfc","payee":"ravi@icici","amount":"250.50"}`
	res, p := post("k1", body)
	if res.StatusCode != 200 || p["state"] != "success" || p["amount"] != "250.50" || p["payee_name"] != "RAVI KUMAR" {
		t.Fatalf("%d %v", res.StatusCode, p)
	}
	if res, q := post("k1", body); res.StatusCode != 200 || q["id"] != p["id"] {
		t.Fatalf("replay: %d %v", res.StatusCode, q)
	}
	if res, _ := post("k1", strings.Replace(body, "250.50", "9", 1)); res.StatusCode != 409 {
		t.Fatalf("key reuse: %d", res.StatusCode)
	}
	if res, _ := post("", body); res.StatusCode != 400 {
		t.Fatalf("no key: %d", res.StatusCode)
	}
	if res, _ := post("k2", `{"payer":"asha@hdfc","payee":"ravi@icici","amount":"1.234"}`); res.StatusCode != 400 {
		t.Fatalf("bad amount: %d", res.StatusCode)
	}
	if res, _ := post("k3", `{"payer":"asha@hdfc","payee":"ravi@icici","amount":"5","kind":"collect"}`); res.StatusCode != 422 {
		t.Fatalf("p2p collect: %d", res.StatusCode)
	}
	get, _ := http.Get(srv.URL + "/payments/" + p["id"])
	if get.StatusCode != 200 {
		t.Fatalf("get: %d", get.StatusCode)
	}
}
