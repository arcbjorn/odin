package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const grokBillingURL = "https://grok.com/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig"

func (p *AccountUsageProvider) grokUsage(ctx context.Context) (*AccountUsage, error) {
	if err := p.requireTokens(); err != nil {
		return nil, err
	}
	resp, err := doTokenRequest(ctx, p.http, p.tokens, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, grokBillingURL, bytes.NewReader([]byte{0, 0, 0, 0, 0}))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", "https://grok.com")
		req.Header.Set("Referer", "https://grok.com/?_s=usage")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Content-Type", "application/grpc-web+proto")
		req.Header.Set("x-grpc-web", "1")
		req.Header.Set("x-user-agent", "connect-es/2.1.1")
		req.Header.Set("User-Agent", "odin/1")
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("grok usage: http %d: %s", resp.StatusCode, truncate(string(payload), 300))
	}
	if err := validateGRPCWebStatus(resp.Header, payload); err != nil {
		return nil, fmt.Errorf("grok usage: %w", err)
	}
	now := time.Now()
	used, reset, err := parseGrokBilling(payload, now)
	if err != nil {
		return nil, fmt.Errorf("decode grok usage: %w", err)
	}
	return &AccountUsage{
		Provider:  p.provider,
		Plan:      "Grok subscription",
		FetchedAt: now,
		Windows: []AccountUsageWindow{{
			Label:       "Monthly",
			UsedPercent: clampPercent(used),
			ResetAt:     reset,
		}},
	}, nil
}

type protoVarint struct {
	path  string
	value uint64
}

type protoFloat struct {
	path  string
	value float64
}

func parseGrokBilling(data []byte, now time.Time) (float64, time.Time, error) {
	frames := grpcWebFrames(data, false)
	if len(frames) == 0 {
		return 0, time.Time{}, fmt.Errorf("response contains no protobuf data frames")
	}
	var varints []protoVarint
	var floats []protoFloat
	for _, frame := range frames {
		scanProtobuf(frame, nil, 0, &varints, &floats)
	}

	var selected *protoFloat
	for i := range floats {
		candidate := &floats[i]
		if candidate.value < 0 || candidate.value > 100 ||
			(candidate.path != "1" && !strings.HasSuffix(candidate.path, ".1")) {
			continue
		}
		if selected == nil || candidate.path == "1.1" && selected.path != "1.1" {
			selected = candidate
		}
	}

	var preferredReset time.Time
	var fallbackReset time.Time
	for _, candidate := range varints {
		if candidate.value < 1_700_000_000 {
			continue
		}
		stamp := time.Unix(int64(candidate.value), 0).UTC()
		if !stamp.After(now) {
			continue
		}
		if fallbackReset.IsZero() || stamp.Before(fallbackReset) {
			fallbackReset = stamp
		}
		if candidate.path == "1.5.1" && (preferredReset.IsZero() || stamp.Before(preferredReset)) {
			preferredReset = stamp
		}
	}
	reset := preferredReset
	if reset.IsZero() {
		reset = fallbackReset
	}
	if selected != nil {
		return selected.value, reset, nil
	}
	if len(floats) == 0 && !reset.IsZero() && hasProtoPrefix(varints, "1.6") {
		return 0, reset, nil
	}
	return 0, time.Time{}, fmt.Errorf("response contains no billing percentage")
}

func scanProtobuf(data []byte, path []int, depth int, varints *[]protoVarint, floats *[]protoFloat) {
	for index := 0; index < len(data); {
		start := index
		key, ok := readProtoVarint(data, &index)
		if !ok || key == 0 {
			index = start + 1
			continue
		}
		field := int(key >> 3)
		wire := int(key & 7)
		fieldPath := append(append([]int(nil), path...), field)
		switch wire {
		case 0:
			value, ok := readProtoVarint(data, &index)
			if !ok {
				index = start + 1
				continue
			}
			*varints = append(*varints, protoVarint{path: protoPath(fieldPath), value: value})
		case 1:
			if index+8 > len(data) {
				return
			}
			index += 8
		case 2:
			length, ok := readProtoVarint(data, &index)
			if !ok || length > uint64(len(data)-index) {
				index = start + 1
				continue
			}
			end := index + int(length)
			if depth < 4 {
				scanProtobuf(data[index:end], fieldPath, depth+1, varints, floats)
			}
			index = end
		case 5:
			if index+4 > len(data) {
				return
			}
			value := float64(math.Float32frombits(binary.LittleEndian.Uint32(data[index : index+4])))
			index += 4
			if !math.IsNaN(value) && !math.IsInf(value, 0) {
				*floats = append(*floats, protoFloat{path: protoPath(fieldPath), value: value})
			}
		default:
			index = start + 1
		}
	}
}

func readProtoVarint(data []byte, index *int) (uint64, bool) {
	var value uint64
	for shift := uint(0); *index < len(data) && shift < 64; shift += 7 {
		b := data[*index]
		(*index)++
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, true
		}
	}
	return 0, false
}

func protoPath(path []int) string {
	parts := make([]string, len(path))
	for i, field := range path {
		parts[i] = strconv.Itoa(field)
	}
	return strings.Join(parts, ".")
}

func hasProtoPrefix(fields []protoVarint, prefix string) bool {
	for _, field := range fields {
		if field.path == prefix || strings.HasPrefix(field.path, prefix+".") {
			return true
		}
	}
	return false
}

func grpcWebFrames(data []byte, trailers bool) [][]byte {
	var frames [][]byte
	for index := 0; index+5 <= len(data); {
		flags := data[index]
		length := int(binary.BigEndian.Uint32(data[index+1 : index+5]))
		start := index + 5
		end := start + length
		if end > len(data) {
			break
		}
		if (flags&0x80 != 0) == trailers {
			frames = append(frames, data[start:end])
		}
		index = end
	}
	return frames
}

func validateGRPCWebStatus(headers http.Header, data []byte) error {
	if status := headers.Get("grpc-status"); status != "" && status != "0" {
		return grpcStatusError(status, headers.Get("grpc-message"))
	}
	for _, trailer := range grpcWebFrames(data, true) {
		values := make(map[string]string)
		for _, line := range strings.Split(string(trailer), "\n") {
			parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
			if len(parts) == 2 {
				values[strings.ToLower(strings.TrimSpace(parts[0]))] = strings.TrimSpace(parts[1])
			}
		}
		if status := values["grpc-status"]; status != "" && status != "0" {
			return grpcStatusError(status, values["grpc-message"])
		}
	}
	return nil
}

func grpcStatusError(status, encodedMessage string) error {
	message, _ := url.QueryUnescape(encodedMessage)
	if message == "" {
		return fmt.Errorf("gRPC status %s", status)
	}
	return fmt.Errorf("gRPC status %s: %s", status, message)
}
