package main

import (
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
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/go-resty/resty/v2"
	"github.com/goccy/go-json"
	"github.com/gofiber/fiber/v2"
	"golang.org/x/sync/singleflight"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	localenv "mensalocalizations/tools/env"
)

var (
	rdb = redis.NewClient(&redis.Options{
		Addr:     localenv.GetRedisAddr(),
		Password: localenv.GetRedisPassword(),
		DB:       0,
	})

	sf singleflight.Group
)

type s3Client struct {
	client *s3.Client
	bucket string
}

func newS3ClientFromEnv(ctx context.Context) (*s3Client, error) {
	bucket := localenv.GetS3Bucket()
	if bucket == "" {
		return nil, errors.New("S3_BUCKET is required")
	}

	region := localenv.GetS3Region()
	endpoint := localenv.GetS3Endpoint()
	accessKey := localenv.GetS3AccessKey()
	secretKey := localenv.GetS3SecretKey()
	forcePathStyle := localenv.GetS3ForcePathStyle()

	if endpoint == "" {
		return nil, errors.New("S3_ENDPOINT is required")
	}
	if accessKey == "" {
		return nil, errors.New("S3_ACCESS_KEY is required")
	}
	if secretKey == "" {
		return nil, errors.New("S3_SECRET_KEY is required")
	}

	cred := credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(cred),
		config.WithRegion(region),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
			prefixedEndpoint := endpoint
			if !strings.Contains(endpoint, "://") {
				prefixedEndpoint = "http://" + endpoint
			}
			return aws.Endpoint{URL: prefixedEndpoint, SigningRegion: region}, nil
		})),
	)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = forcePathStyle
	})

	return &s3Client{client: client, bucket: bucket}, nil
}

func (s *s3Client) getLatest(ctx context.Context, appID, lang string) ([]byte, error) {
	key := s3LatestKey(appID, lang)
	log.Printf("[cache][s3] GET latest key=%q bucket=%q", key, s.bucket)

	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("[cache][s3] MISS/ERROR latest key=%q err=%v", key, err)
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()

	b, err := io.ReadAll(out.Body)
	if err != nil {
		log.Printf("[cache][s3] ERROR read body key=%q err=%v", key, err)
		return nil, err
	}
	log.Printf("[cache][s3] HIT latest key=%q bytes=%d", key, len(b))
	return b, nil
}

func (s *s3Client) putVersionAndLatest(ctx context.Context, appID, lang string, payload []byte) error {
	hash := sha256.Sum256(payload)
	hashHex := hex.EncodeToString(hash[:])

	now := time.Now().UTC().Format("20060102T150405Z")
	versionKey := s3VersionKey(appID, lang, now, hashHex)
	latestKey := s3LatestKey(appID, lang)

	contentType := aws.String("application/json")
	meta := map[string]string{
		"app":         appID,
		"lang":        lang,
		"sha256":      hashHex,
		"created_utc": now,
		"source":      "tolgee",
	}

	// 1) Scrive oggetto immutabile versionato
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(versionKey),
		Body:        bytes.NewReader(payload),
		ContentType: contentType,
		Metadata:    meta,
		ACL:         types.ObjectCannedACLPrivate,
	})
	if err != nil {
		return err
	}

	// 2) Aggiorna puntatore latest (sovrascrive)
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(latestKey),
		Body:        bytes.NewReader(payload),
		ContentType: contentType,
		Metadata:    meta,
		ACL:         types.ObjectCannedACLPrivate,
	})
	return err
}

// Cache keys per languages Tolgee
func s3LanguagesLatestKey(appID string) string {
	return fmt.Sprintf("tolgee-languages/%s/latest.json", sanitizeKeyPart(appID))
}

func s3LanguagesVersionKey(appID, tsUTC, sha string) string {
	return fmt.Sprintf("tolgee-languages/%s/%s_%s.json", sanitizeKeyPart(appID), tsUTC, sha)
}

func (s *s3Client) getLatestLanguages(ctx context.Context, appID string) ([]byte, error) {
	key := s3LanguagesLatestKey(appID)
	log.Printf("[cache][s3] GET languages latest key=%q bucket=%q", key, s.bucket)

	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("[cache][s3] MISS/ERROR languages latest key=%q err=%v", key, err)
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()

	b, err := io.ReadAll(out.Body)
	if err != nil {
		log.Printf("[cache][s3] ERROR read languages body key=%q err=%v", key, err)
		return nil, err
	}
	log.Printf("[cache][s3] HIT languages latest key=%q bytes=%d", key, len(b))
	return b, nil
}

func (s *s3Client) putLanguagesVersionAndLatest(ctx context.Context, appID string, payload []byte) error {
	hash := sha256.Sum256(payload)
	hashHex := hex.EncodeToString(hash[:])

	now := time.Now().UTC().Format("20060102T150405Z")
	versionKey := s3LanguagesVersionKey(appID, now, hashHex)
	latestKey := s3LanguagesLatestKey(appID)

	contentType := aws.String("application/json")
	meta := map[string]string{
		"app":         appID,
		"sha256":      hashHex,
		"created_utc": now,
		"source":      "tolgee",
		"endpoint":    "languages",
	}

	// 1) Scrive oggetto immutabile versionato
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(versionKey),
		Body:        bytes.NewReader(payload),
		ContentType: contentType,
		Metadata:    meta,
		ACL:         types.ObjectCannedACLPrivate,
	})
	if err != nil {
		return err
	}

	// 2) Aggiorna latest
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(latestKey),
		Body:        bytes.NewReader(payload),
		ContentType: contentType,
		Metadata:    meta,
		ACL:         types.ObjectCannedACLPrivate,
	})
	return err
}

const (
	redisFetchedAtSuffix = ":fetched_utc" // unix seconds
	redisValueTTL        = 10 * time.Minute
)

// parseUnixSeconds is still used for backward compatibility logs; kept minimal.
func parseUnixSeconds(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(i, 0).UTC(), true
}

// getTranslationsCached:
// 1) prova Redis (best-effort)
// 2) se Redis non disponibile o miss: prova S3 latest (best-effort)
// 3) se S3 non disponibile o miss: prova Tolgee (timeout 10s)
// 4) se Tolgee va ok: salva Redis + crea nuova versione su S3 + aggiorna latest (best-effort)
// 5) se tutto fallisce: ritorna JSON vuoto
func getTranslationsCached(ctx context.Context, s3c *s3Client, appID, lang string, nested bool, force bool) ([]byte, error) {
	mode := "flat"
	if nested {
		mode = "nested"
	}

	// Include la modalità nella cache per evitare collisioni tra output diversi
	redisKey := "translations:" + appID + ":" + lang + ":" + mode
	fetchedAtKey := redisKey + redisFetchedAtSuffix

	log.Printf("[cache][translations] request app=%q lang=%q mode=%q redisKey=%q", appID, lang, mode, redisKey)

	if !force {
		b, err := rdb.Get(ctx, redisKey).Bytes()
		if err == nil {
			log.Printf("[cache][redis] HIT key=%q bytes=%d", redisKey, len(b))
			return b, nil
		}
		if err == redis.Nil {
			log.Printf("[cache][redis] MISS key=%q", redisKey)
		} else {
			log.Printf("[cache][redis] ERROR key=%q err=%v (best-effort)", redisKey, err)
		}
	}

	v, sfErr, _ := sf.Do(redisKey, func() (interface{}, error) {
		log.Printf("[cache][singleflight] computing key=%q", redisKey)
		if !force {
			bb, e := rdb.Get(ctx, redisKey).Bytes()
			if e == nil {
				log.Printf("[cache][redis] HIT (2nd check) key=%q bytes=%d", redisKey, len(bb))
				return bb, nil
			}
			if e == redis.Nil {
				log.Printf("[cache][redis] MISS (2nd check) key=%q", redisKey)
			} else {
				log.Printf("[cache][redis] ERROR (2nd check) key=%q err=%v", redisKey, e)
			}

			if s3c != nil {
				fallback, s3err := s3c.getLatest(ctx, appID, lang+"_"+mode)
				if s3err == nil && len(fallback) > 0 {
					_now := time.Now().UTC()
					log.Printf("[cache][s3] using fallback for translations app=%q lang=%q mode=%q -> write redis key=%q ttl=%s", appID, lang, mode, redisKey, redisValueTTL)
					_ = rdb.Set(ctx, redisKey, fallback, redisValueTTL).Err()
					_ = rdb.Set(ctx, fetchedAtKey, strconv.FormatInt(_now.Unix(), 10), redisValueTTL).Err()

					return fallback, nil
				}
				if s3err != nil {
					log.Printf("[cache][s3] fallback failed for translations app=%q lang=%q mode=%q err=%v", appID, lang, mode, s3err)
				}
			} else {
				log.Printf("[cache][s3] disabled (no client) -> skip translations fallback")
			}
		}

		// Ultima risorsa: Tolgee (timeout 10s)
		tolgeeURL := "https://app.tolgee.io/v2/projects/export"
		tolgeeAK := appID
		if tolgeeAK == "" {
			log.Printf("[cache][tolgee] SKIP export: empty ak (appID)")
			return []byte("{}"), nil
		}

		log.Printf("[cache][tolgee] GET export url=%q ak=%q lang=%q nested=%v", tolgeeURL, tolgeeAK, lang, nested)

		client := resty.New().
			SetTimeout(10 * time.Second).
			SetRetryCount(0)

		req := client.R().
			SetContext(ctx).
			SetQueryParams(map[string]string{
				"ak":        tolgeeAK,
				"size":      "1000",
				"languages": lang,
				"format":    "JSON",
				"zip":       "false",
			})

		if !nested {
			req.SetQueryParam("structureDelimiter", "")
		}

		resp, e := req.Get(tolgeeURL)

		if e != nil {
			log.Printf("[cache][tolgee] ERROR export ak=%q lang=%q err=%v", tolgeeAK, lang, e)
			return []byte("{}"), nil
		}

		if resp.StatusCode() < http.StatusOK || resp.StatusCode() >= http.StatusMultipleChoices {
			log.Printf("[cache][tolgee] ERROR export non-2xx ak=%q lang=%q status=%d", tolgeeAK, lang, resp.StatusCode())
			return []byte("{}"), nil
		}

		body := resp.Body()
		log.Printf("[cache][tolgee] OK export ak=%q lang=%q bytes=%d", tolgeeAK, lang, len(body))

		if len(body) == 0 {
			log.Printf("[cache][tolgee] WARN empty body export ak=%q lang=%q", tolgeeAK, lang)
			return []byte("{}"), nil
		}

		_now := time.Now().UTC()
		log.Printf("[cache][redis] SET translations key=%q ttl=%s bytes=%d", redisKey, redisValueTTL, len(body))
		_ = rdb.Set(ctx, redisKey, body, redisValueTTL).Err()
		_ = rdb.Set(ctx, fetchedAtKey, strconv.FormatInt(_now.Unix(), 10), redisValueTTL).Err()

		if s3c != nil {
			log.Printf("[cache][s3] write-back translations app=%q lang=%q mode=%q", appID, lang, mode)
			_ = s3c.putVersionAndLatest(ctx, appID, lang+"_"+mode, body)
		}

		return body, nil
	})
	if sfErr != nil {
		log.Printf("[cache][singleflight] ERROR key=%q err=%v (best-effort)", redisKey, sfErr)
		return []byte("{}"), nil
	}

	bb, ok := v.([]byte)
	if !ok {
		log.Printf("[cache] ERROR singleflight value type mismatch key=%q", redisKey)
		return []byte("{}"), nil
	}
	log.Printf("[cache] DONE translations key=%q bytes=%d", redisKey, len(bb))
	return bb, nil
}

// getLanguagesCached:
// 1) prova Redis (best-effort)
// 2) se Redis non disponibile o miss: prova S3 latest (best-effort)
// 3) se S3 non disponibile o miss: prova Tolgee (timeout 10s)
// 4) se Tolgee va ok: salva Redis + crea nuova versione su S3 + aggiorna latest (best-effort)
// 5) se tutto fallisce: ritorna JSON vuoto
func getLanguagesCached(ctx context.Context, s3c *s3Client, appID string) ([]byte, error) {
	redisKey := "languages:" + appID
	fetchedAtKey := redisKey + redisFetchedAtSuffix
	log.Printf("[cache][languages] request app=%q redisKey=%q", appID, redisKey)

	b, err := rdb.Get(ctx, redisKey).Bytes()
	if err == nil {
		log.Printf("[cache][redis] HIT key=%q bytes=%d", redisKey, len(b))
		return b, nil
	}
	if err == redis.Nil {
		log.Printf("[cache][redis] MISS key=%q", redisKey)
	} else {
		log.Printf("[cache][redis] ERROR key=%q err=%v (best-effort)", redisKey, err)
	}

	v, sfErr, _ := sf.Do(redisKey, func() (interface{}, error) {
		log.Printf("[cache][singleflight] computing key=%q", redisKey)
		bb, e := rdb.Get(ctx, redisKey).Bytes()
		if e == nil {
			log.Printf("[cache][redis] HIT (2nd check) key=%q bytes=%d", redisKey, len(bb))
			return bb, nil
		}
		if e == redis.Nil {
			log.Printf("[cache][redis] MISS (2nd check) key=%q", redisKey)
		} else {
			log.Printf("[cache][redis] ERROR (2nd check) key=%q err=%v", redisKey, e)
		}

		if s3c != nil {
			fallback, s3err := s3c.getLatestLanguages(ctx, appID)
			if s3err == nil && len(fallback) > 0 {
				_now := time.Now().UTC()
				log.Printf("[cache][s3] using fallback for languages app=%q -> write redis key=%q ttl=%s", appID, redisKey, redisValueTTL)
				_ = rdb.Set(ctx, redisKey, fallback, redisValueTTL).Err()
				_ = rdb.Set(ctx, fetchedAtKey, strconv.FormatInt(_now.Unix(), 10), redisValueTTL).Err()

				return fallback, nil
			}
			if s3err != nil {
				log.Printf("[cache][s3] fallback failed for languages app=%q err=%v", appID, s3err)
			}
		} else {
			log.Printf("[cache][s3] disabled (no client) -> skip languages fallback")
		}

		// Tolgee
		tolgeeAK := appID
		if tolgeeAK == "" {
			log.Printf("[cache][tolgee] SKIP languages: empty ak (appID)")
			return []byte("{}"), nil
		}

		url := "https://app.tolgee.io/v2/projects/languages"
		log.Printf("[cache][tolgee] GET languages url=%q ak=%q", url, tolgeeAK)

		client := resty.New().
			SetTimeout(10 * time.Second).
			SetRetryCount(0)

		resp, e := client.R().
			SetContext(ctx).
			SetQueryParams(map[string]string{
				"ak":   tolgeeAK,
				"size": "1000",
			}).
			Get(url)

		if e != nil {
			log.Printf("[cache][tolgee] ERROR languages ak=%q err=%v", tolgeeAK, e)
			return []byte("{}"), nil
		}
		if resp.StatusCode() < http.StatusOK || resp.StatusCode() >= http.StatusMultipleChoices {
			log.Printf("[cache][tolgee] ERROR languages non-2xx ak=%q status=%d", tolgeeAK, resp.StatusCode())
			return []byte("{}"), nil
		}

		body := resp.Body()
		log.Printf("[cache][tolgee] OK languages ak=%q bytes=%d", tolgeeAK, len(body))

		if len(body) == 0 {
			log.Printf("[cache][tolgee] WARN empty body languages ak=%q", tolgeeAK)
			return []byte("{}"), nil
		}

		_now := time.Now().UTC()
		log.Printf("[cache][redis] SET languages key=%q ttl=%s bytes=%d", redisKey, redisValueTTL, len(body))
		_ = rdb.Set(ctx, redisKey, body, redisValueTTL).Err()
		_ = rdb.Set(ctx, fetchedAtKey, strconv.FormatInt(_now.Unix(), 10), redisValueTTL).Err()

		if s3c != nil {
			log.Printf("[cache][s3] write-back languages app=%q", appID)
			_ = s3c.putLanguagesVersionAndLatest(ctx, appID, body)
		}

		return body, nil
	})
	if sfErr != nil {
		log.Printf("[cache][singleflight] ERROR key=%q err=%v (best-effort)", redisKey, sfErr)
		return []byte("{}"), nil
	}

	bb, ok := v.([]byte)
	if !ok {
		log.Printf("[cache] ERROR singleflight value type mismatch key=%q", redisKey)
		return []byte("{}"), nil
	}
	log.Printf("[cache] DONE languages key=%q bytes=%d", redisKey, len(bb))
	return bb, nil
}

type tolgeeLanguage struct {
	Tag string `json:"tag"`
}

type updateSummary struct {
	App        string   `json:"app"`
	Languages  []string `json:"languages"`
	Refreshed  int      `json:"refreshed"`
	Failures   []string `json:"failures,omitempty"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at"`
}

type tolgeeSignatureHeader struct {
	Timestamp int64  `json:"timestamp"`
	Signature string `json:"signature"`
}

type idMappings struct {
	appIDs    map[string]string
	appSecret map[string]string
}

func parseMappings(raw string) map[string]string {
	m := make(map[string]string)
	if raw == "" {
		return m
	}
	parts := strings.Split(raw, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		kv := strings.SplitN(p, ":", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		if key != "" && val != "" {
			m[key] = val
		}
	}
	return m
}

func loadIDMappings() idMappings {
	return idMappings{
		appIDs:    parseMappings(localenv.GetAppIDsMappingRaw()),
		appSecret: parseMappings(localenv.GetAppSecretsMappingRaw()),
	}
}

func resolveAppID(idOrApp string, maps idMappings) (string, bool) {
	if val, ok := maps.appIDs[idOrApp]; ok {
		return val, true
	}
	return idOrApp, false
}

func resolveSecret(idOrApp string, maps idMappings) string {
	if val, ok := maps.appSecret[idOrApp]; ok {
		return val
	}
	return localenv.GetWebhookSecret()
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

func refreshAppTranslations(ctx context.Context, s3c *s3Client, appID string) updateSummary {
	log.Printf("[update] starting refresh for app=%q", appID)
	started := time.Now().UTC()
	summary := updateSummary{App: appID, StartedAt: started.Format(time.RFC3339)}

	langsPayload, err := getLanguagesCached(ctx, s3c, appID)
	if err != nil {
		summary.Failures = append(summary.Failures, "languages fetch failed: "+err.Error())
		summary.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		return summary
	}

	var langs []tolgeeLanguage
	if e := json.Unmarshal(langsPayload, &langs); e != nil {
		summary.Failures = append(summary.Failures, "languages decode failed: "+e.Error())
		summary.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		return summary
	}
	for _, l := range langs {
		if strings.TrimSpace(l.Tag) == "" {
			continue
		}
		summary.Languages = append(summary.Languages, l.Tag)
		if _, e := getTranslationsCached(ctx, s3c, appID, l.Tag, false, true); e != nil {
			summary.Failures = append(summary.Failures, fmt.Sprintf("%s flat: %v", l.Tag, e))
		} else {
			summary.Refreshed++
		}
		if _, e := getTranslationsCached(ctx, s3c, appID, l.Tag, true, true); e != nil {
			summary.Failures = append(summary.Failures, fmt.Sprintf("%s nested: %v", l.Tag, e))
		} else {
			summary.Refreshed++
		}
	}

	summary.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	return summary
}

func main() {
	rootCtx := context.Background()
	var s3c *s3Client
	if localenv.GetS3Enabled() {
		c, err := newS3ClientFromEnv(rootCtx)
		if err != nil {
			log.Printf("[cache][s3] disabled (config error): %v", err)
		} else {
			log.Printf("[cache][s3] enabled bucket=%q", c.bucket)
			s3c = c
		}
	} else {
		log.Printf("[cache][s3] disabled via env S3_ENABLED=false")
	}

	mappings := loadIDMappings()

	app := fiber.New(fiber.Config{
		JSONEncoder: json.Marshal,
		JSONDecoder: json.Unmarshal,
		Prefork:     true,
	})

	app.Use(func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		duration := time.Since(start)
		c.Append("Server-Timing", "app;dur="+strconv.FormatInt(duration.Milliseconds(), 10)+"ms")
		return err
	})

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.Status(200).SendString("ok")
	})

	app.All("/api/:app/update", func(c *fiber.Ctx) error {
		appParam := c.Params("app")
		appID, mapped := resolveAppID(appParam, mappings)
		if !mapped {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "updates allowed only for numeric mapped ids"})
		}
		secret := resolveSecret(appParam, mappings)
		header := c.Get("Tolgee-Signature")
		body := c.Body()
		if !verifyTolgeeSignature(secret, header, body) {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid webhook signature"})
		}

		summary := refreshAppTranslations(c.Context(), s3c, appID)
		return c.Status(http.StatusOK).JSON(summary)
	})

	app.Get("/api/:app", func(c *fiber.Ctx) error {
		appParam := c.Params("app")
		appID, _ := resolveAppID(appParam, mappings)

		data, err := getLanguagesCached(c.Context(), s3c, appID)
		if err != nil {
			data = []byte("{}")
		}

		c.Set("Content-type", "application/json; charset=utf-8")
		return c.Status(200).Send(data)
	})
	app.Get("/api/:app/:lang", func(c *fiber.Ctx) error {
		appParam := c.Params("app")
		appID, _ := resolveAppID(appParam, mappings)
		lang := c.Params("lang")

		nested := false
		if raw := c.Query("nested", "false"); raw != "" {
			b, perr := strconv.ParseBool(raw)
			if perr == nil {
				nested = b
			}
		}

		data, err := getTranslationsCached(c.Context(), s3c, appID, lang, nested, false)
		if err != nil {
			data = []byte("{}")
		}

		c.Set("Content-type", "application/json; charset=utf-8")
		return c.Status(200).Send(data)
	})

	log.Fatal(app.Listen(":3000"))
}
