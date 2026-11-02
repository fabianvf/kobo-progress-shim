// Package kobostore looks up book metadata from Kobo's store, using the
// device's own auth for that one request.
package kobostore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"
)

const baseURL = "https://storeapi.kobo.com"

var ErrNotFound = errors.New("kobostore: not in store library")

var entitlementID = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

type Metadata struct {
	Title        string   `json:"Title"`
	Contributors []string `json:"Contributors"`
	ISBN         string   `json:"Isbn"`
}

type Client struct {
	baseURL string
	http    *http.Client
}

func New() *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: 15 * time.Second}}
}

// Metadata fetches a book by entitlement id, sending the device's headers. CWA's
// own books aren't in the store, so they should return ErrNotFound. Errors
// include the store's own error text but never the headers sent.
func (c *Client) Metadata(ctx context.Context, id string, device http.Header) (Metadata, error) {
	if !entitlementID.MatchString(id) {
		return Metadata{}, fmt.Errorf("kobostore: invalid id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/library/"+id+"/metadata", nil)
	if err != nil {
		return Metadata{}, err
	}
	req.Header = device.Clone()
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Metadata{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Metadata{}, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return Metadata{}, statusError(resp)
	}
	var books []Metadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&books); err != nil {
		return Metadata{}, fmt.Errorf("kobostore: decoding response: %w", err)
	}
	if len(books) == 0 {
		return Metadata{}, ErrNotFound
	}
	return books[0], nil
}

// statusError keeps only the store's error code and message, so an unexpected
// body can't put anything else in the logs.
func statusError(resp *http.Response) error {
	var body struct {
		ResponseStatus struct {
			ErrorCode string `json:"ErrorCode"`
			Message   string `json:"Message"`
		} `json:"ResponseStatus"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	message := body.ResponseStatus.Message
	if len(message) > 200 {
		message = message[:200]
	}
	return fmt.Errorf("kobostore: status %d, server %q, %s: %s", resp.StatusCode, resp.Header.Get("Server"), body.ResponseStatus.ErrorCode, message)
}
