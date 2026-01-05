package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/goccy/go-json"
)

type tolgeeSignatureHeader struct {
	Timestamp int64  `json:"timestamp"`
	Signature string `json:"signature"`
}

// GetLanguages calls Tolgee /languages endpoint and returns the raw JSON body.
func GetLanguages(ctx context.Context, appKey string) (*TolgeeModel, []byte, error) {
	if appKey == "" {
		return nil, nil, errors.New("tolgee app key is required")
	}

	url := "https://app.tolgee.io/v2/projects/languages"
	client := resty.New().
		SetTimeout(0).
		SetRetryCount(0)

	resp, err := client.R().
		SetContext(ctx).
		SetResult(&TolgeeModel{}).
		SetQueryParams(map[string]string{
			"ak":   appKey,
			"size": "1000",
		}).
		Get(url)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode() < http.StatusOK || resp.StatusCode() >= http.StatusMultipleChoices {
		return nil, nil, fmt.Errorf("tolgee languages non-2xx: status=%d", resp.StatusCode())
	}
	return resp.Result().(*TolgeeModel), resp.Body(), nil
}

// GetTranslations calls Tolgee /export endpoint for a specific language.
// It requests a ZIP archive and returns its files as a map[name][]byte.
// If nested is false, structureDelimiter is set to flatten the output.
func GetTranslations(ctx context.Context, appKey, lang string, nested bool) (map[string][]byte, error) {
	if appKey == "" {
		return nil, errors.New("tolgee app key is required")
	}
	if lang == "" {
		return nil, errors.New("language tag is required")
	}

	url := "https://app.tolgee.io/v2/projects/export"
	client := resty.New().
		SetTimeout(0).
		SetRetryCount(0)

	req := client.R().
		SetContext(ctx).
		SetQueryParams(map[string]string{
			"ak":        appKey,
			"size":      "1000",
			"languages": lang,
			"format":    "JSON",
			"zip":       "true",
		})

	if !nested {
		req.SetQueryParam("structureDelimiter", "")
	}

	resp, err := req.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() < http.StatusOK || resp.StatusCode() >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("tolgee export non-2xx: status=%d", resp.StatusCode())
	}

	zipBytes := resp.Body()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, fmt.Errorf("invalid zip response: %w", err)
	}

	files := make(map[string][]byte)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("zip open %s: %w", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("zip read %s: %w", f.Name, err)
		}
		files[strings.ReplaceAll(f.Name, ".json", "")] = data
	}

	return files, nil
}

func verifyTolgeeSignature(secret string, rawHeader string, body []byte) bool {
	if secret == "" || rawHeader == "" {
		return false
	}
	var hdr tolgeeSignatureHeader
	if err := json.Unmarshal([]byte(rawHeader), &hdr); err != nil {
		log.Printf("[webhook] signature header unmarshal error: %v", err)
		return false
	}
	if hdr.Timestamp <= 0 || hdr.Signature == "" {
		return false
	}

	signedPayload := fmt.Sprintf("%d.%s", hdr.Timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signedPayload))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(hdr.Signature)) {
		log.Printf("[webhook] signature mismatch")
		return false
	}

	// timestamp in ms, reject older than 5 minutes
	if hdr.Timestamp < time.Now().Add(-5*time.Minute).UnixMilli() {
		log.Printf("[webhook] signature too old ts=%d", hdr.Timestamp)
		return false
	}
	return true
}

func GetAllLanguagesAndTranslations(ctx context.Context, appKey string, nested bool) (map[string][]byte, error) {
	languages, _, err := GetLanguages(ctx, appKey)
	if err != nil {
		return nil, fmt.Errorf("GetLanguages: %w", err)
	}

	listOfLangs := []string{}
	for _, lang := range languages.Embedded.Languages {
		listOfLangs = append(listOfLangs, lang.Tag)
	}
	listOfLangsStrings := strings.Join(listOfLangs, ", ")
	resp, err := GetTranslations(ctx, appKey, listOfLangsStrings, nested)
	if err != nil {
		return nil, fmt.Errorf("GetTranslations: %w", err)
	}
	return resp, nil
}
