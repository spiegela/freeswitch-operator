package calltrace

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LiveKitEvent is the part of a LiveKit webhook (livekit.WebhookEvent, as JSON) the tracer reads.
type LiveKitEvent struct {
	Event       string              `json:"event"`
	ID          string              `json:"id"`
	CreatedAt   jsonInt             `json:"createdAt"`
	Room        *LiveKitRoom        `json:"room"`
	Participant *LiveKitParticipant `json:"participant"`
	Track       *LiveKitTrack       `json:"track"`
}

// LiveKitRoom is a webhook's room.
type LiveKitRoom struct {
	SID          string  `json:"sid"`
	Name         string  `json:"name"`
	CreationTime jsonInt `json:"creationTime"`
}

// LiveKitParticipant is a webhook's participant.
type LiveKitParticipant struct {
	SID        string            `json:"sid"`
	Identity   string            `json:"identity"`
	Kind       string            `json:"kind"`
	Attributes map[string]string `json:"attributes"`
	JoinedAt   jsonInt           `json:"joinedAt"`
	JoinedAtMs jsonInt           `json:"joinedAtMs"`
}

// LiveKitTrack is a webhook's track.
type LiveKitTrack struct {
	SID    string `json:"sid"`
	Type   string `json:"type"`
	Source string `json:"source"`
}

// joined is when the participant joined, as precisely as the event says.
func (p *LiveKitParticipant) joined() time.Time {
	if p.JoinedAtMs > 0 {
		return time.UnixMilli(int64(p.JoinedAtMs))
	}
	if p.JoinedAt > 0 {
		return time.Unix(int64(p.JoinedAt), 0)
	}
	return time.Time{}
}

// jsonInt reads an int64 that protobuf's JSON encoding writes as a string.
type jsonInt int64

func (n *jsonInt) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	v, err := strconv.ParseInt(string(b), 10, 64)
	*n = jsonInt(v)
	return err
}

// LiveKitVerifier checks a webhook's signature: LiveKit signs each with a JWT from the API key and secret, whose
// sha256 claim is the body's hash.
type LiveKitVerifier struct {
	// Keys maps API keys to their secrets.
	Keys map[string]string
	Now  func() time.Time
}

// ParseLiveKitKeys reads keys in LiveKit's LIVEKIT_KEYS form: "key: secret" pairs, one per line or comma-separated.
func ParseLiveKitKeys(s string) (map[string]string, error) {
	keys := map[string]string{}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		if strings.TrimSpace(part) == "" {
			continue
		}
		k, v, ok := strings.Cut(part, ":")
		if !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return nil, errors.New(`want "key: secret" pairs`)
		}
		keys[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return keys, nil
}

// Verify checks the Authorization header's token against body and returns the decoded event.
func (v *LiveKitVerifier) Verify(auth string, body []byte) (*LiveKitEvent, error) {
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("no signed token")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	var claims struct {
		Iss    string  `json:"iss"`
		Exp    float64 `json:"exp"`
		Nbf    float64 `json:"nbf"`
		SHA256 string  `json:"sha256"`
	}
	if err := decodeSegment(parts[0], &header); err != nil || header.Alg != "HS256" {
		return nil, errors.New("token is not HS256")
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return nil, fmt.Errorf("token claims: %w", err)
	}
	secret, ok := v.Keys[claims.Iss]
	if !ok {
		return nil, fmt.Errorf("unknown API key %q", claims.Iss)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, errors.New("bad token signature")
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	t := float64(now().Unix())
	if (claims.Exp != 0 && t > claims.Exp+60) || (claims.Nbf != 0 && t < claims.Nbf-60) {
		return nil, errors.New("token expired or not yet valid")
	}
	sum := sha256.Sum256(body)
	if claims.SHA256 != base64.StdEncoding.EncodeToString(sum[:]) {
		return nil, errors.New("body does not match the token's hash")
	}
	var ev LiveKitEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, fmt.Errorf("webhook body: %w", err)
	}
	return &ev, nil
}

func decodeSegment(s string, into any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// SignLiveKitWebhook signs body as LiveKit does, for tests and for replaying webhooks.
func SignLiveKitWebhook(key, secret string, body []byte, now time.Time) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	sum := sha256.Sum256(body)
	head := enc(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." + enc(map[string]any{
		"iss": key, "nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"sha256": base64.StdEncoding.EncodeToString(sum[:]),
	})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(head))
	return head + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
