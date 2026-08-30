package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"os"
)

func main() {
	secret := []byte(os.Getenv("HMAC_SECRET"))
	if len(secret) == 0 {
		log.Fatal("HMAC_SECRET is required")
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":9000"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Read the raw body BEFORE any JSON parsing — the signature is
		// computed over these exact bytes, not a re-serialized object.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}

		expected := sign(body, secret)
		got := r.Header.Get("X-Webhook-Signature")
		valid := hmac.Equal([]byte(expected), []byte(got))

		log.Printf("webhook id=%s signature_valid=%v body=%q", r.Header.Get("X-Webhook-Id"), valid, body)
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("testreceiver listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func sign(payload, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
