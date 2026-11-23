package psp

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/chillprakash/upi-ledger/internal/money"
)

// Handler serves the app's payment API:
//
//	POST /payments        body {"payer","payee","amount":"100.50","kind"}; header Idempotency-Key
//	GET  /payments/{id}
func (e *Engine) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /payments", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			writeErr(w, http.StatusBadRequest, "Idempotency-Key header is required")
			return
		}
		var body struct {
			Payer, Payee, Amount, Kind string
			Merchant                   bool
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		amt, err := money.Parse(body.Amount)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		p, err := e.Create(r.Context(), key, Request{Payer: body.Payer, Payee: body.Payee, Amount: amt, Kind: Kind(body.Kind), Merchant: body.Merchant})
		switch {
		case errors.Is(err, ErrIdempotencyMismatch):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrTryAgain):
			writeErr(w, http.StatusServiceUnavailable, err.Error())
		case errors.Is(err, ErrUnknownPayee):
			writeErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
		default:
			writeJSON(w, http.StatusOK, view(p))
		}
	})
	mux.HandleFunc("GET /payments/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := e.Get(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "payment not found")
			return
		}
		writeJSON(w, http.StatusOK, view(p))
	})
	return mux
}

type paymentView struct {
	ID        string `json:"id"`
	TxnID     string `json:"txn_id"`
	Payer     string `json:"payer"`
	Payee     string `json:"payee"`
	PayeeName string `json:"payee_name"`
	Amount    string `json:"amount"`
	State     State  `json:"state"`
	Code      string `json:"code,omitempty"`
}

func view(p Payment) paymentView {
	return paymentView{p.ID, p.TxnID, p.Request.Payer, p.Request.Payee, p.PayeeName, p.Request.Amount.String(), p.State, p.Code}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
