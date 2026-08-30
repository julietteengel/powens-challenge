package httpclient

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"powens-challenge/internal/domain"
)

type Deliverer struct {
	client *http.Client
	secret []byte
}

func NewDeliverer(timeout time.Duration, secret []byte) *Deliverer {
	return &Deliverer{client: &http.Client{Timeout: timeout}, secret: secret}
}

func (d *Deliverer) Deliver(ctx context.Context, job *domain.Job) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.DestinationURL, bytes.NewReader(job.Payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Id", job.ID)
	req.Header.Set("X-Webhook-Signature", "sha256="+sign(job.Payload, d.secret))

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

func sign(payload, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
