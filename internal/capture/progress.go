package capture

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
)

// Progress is one book's state from a successful state PUT. Headers holds the
// Kobo's own auth, for a store lookup in memory. It must never be logged or
// stored, so don't print this struct whole.
type Progress struct {
	EntitlementID string
	Percent       int
	Status        string
	Source        string
	LastModified  string
	Headers       http.Header
}

// deviceHeaders keeps everything the device sent, as CWA does when it forwards a
// request to the store, except what describes the hop to this proxy, CWA's own
// session, or the PUT's body. Accept-Encoding goes too, because setting it would
// stop Go decoding the response.
func deviceHeaders(in http.Header) http.Header {
	out := http.Header{}
	for name, values := range in {
		switch {
		case strings.HasPrefix(name, "X-Forwarded-"), strings.HasPrefix(name, "Content-"):
		case name == "Forwarded", name == "X-Real-Ip", name == "Cookie", name == "Accept-Encoding", name == "Connection":
		default:
			out[name] = values
		}
	}
	return out
}

var statePath = regexp.MustCompile(`^/kobo/[^/]+/v1/library/([0-9a-fA-F-]{36})/state(\?|$)`)

// progressFrom returns what a state PUT reports, or nothing for any other
// exchange. A failed PUT is ignored because the Kobo will send it again.
func progressFrom(e *Exchange) []Progress {
	if e.Method != "PUT" || e.Status < 200 || e.Status >= 300 {
		return nil
	}
	path := statePath.FindStringSubmatch(e.RequestURI)
	if path == nil {
		return nil
	}
	raw, truncated := e.RequestBody.snapshot()
	if truncated {
		return nil
	}
	var body struct {
		ReadingStates []struct {
			EntitlementID   string `json:"EntitlementId"`
			CurrentBookmark *struct {
				ProgressPercent float64 `json:"ProgressPercent"`
				Location        *struct {
					Source string `json:"Source"`
				} `json:"Location"`
			} `json:"CurrentBookmark"`
			StatusInfo *struct {
				Status       string `json:"Status"`
				LastModified string `json:"LastModified"`
			} `json:"StatusInfo"`
		} `json:"ReadingStates"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil
	}
	var out []Progress
	for _, state := range body.ReadingStates {
		if state.EntitlementID != path[1] {
			continue
		}
		p := Progress{
			EntitlementID: state.EntitlementID,
			Headers:       deviceHeaders(e.RequestHeader),
		}
		if state.CurrentBookmark != nil {
			p.Percent = int(state.CurrentBookmark.ProgressPercent)
			if state.CurrentBookmark.Location != nil {
				p.Source = state.CurrentBookmark.Location.Source
			}
		}
		if state.StatusInfo != nil {
			p.Status = state.StatusInfo.Status
			p.LastModified = state.StatusInfo.LastModified
		}
		out = append(out, p)
	}
	return out
}

func (r *Recorder) observe(exchange *Exchange) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("progress observer failed: %T", recovered)
		}
	}()
	if r.OnProgress == nil {
		return
	}
	for _, p := range progressFrom(exchange) {
		r.OnProgress(p)
	}
}
