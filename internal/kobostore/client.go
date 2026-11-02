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

// Metadata fetches a book by entitlement id. CWA's own books aren't in the
// store, so they return ErrNotFound. Errors never include the auth header.
func (c *Client) Metadata(ctx context.Context, id, authorization, userAgent string) (Metadata, error) {
	if !entitlementID.MatchString(id) {
		return Metadata{}, fmt.Errorf("kobostore: invalid id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/library/"+id+"/metadata", nil)
	if err != nil {
		return Metadata{}, err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return Metadata{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Metadata{}, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return Metadata{}, fmt.Errorf("kobostore: status %d", resp.StatusCode)
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
