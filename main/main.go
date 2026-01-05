package main

import (
	"bytes"
	"context"
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
	staleAfter           = 15 * time.Minute
	redisValueTTL        = 10 * time.Minute
)

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

func (s *s3Client) headCreatedUTC(ctx context.Context, key string) (time.Time, bool) {
	log.Printf("[cache][s3] HEAD key=%q bucket=%q (read created_utc)", key, s.bucket)
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("[cache][s3] HEAD failed key=%q err=%v", key, err)
		return time.Time{}, false
	}
	if out.Metadata == nil {
		log.Printf("[cache][s3] HEAD ok but no metadata key=%q", key)
		return time.Time{}, false
	}
	raw := out.Metadata["created_utc"]
	if raw == "" {
		log.Printf("[cache][s3] HEAD ok but created_utc missing key=%q", key)
		return time.Time{}, false
	}
	t, err := time.Parse("20060102T150405Z", raw)
	if err != nil {
		log.Printf("[cache][s3] HEAD created_utc parse error key=%q raw=%q err=%v", key, raw, err)
		return time.Time{}, false
	}
	log.Printf("[cache][s3] HEAD created_utc key=%q created_utc=%s", key, t.UTC().Format(time.RFC3339))
	return t.UTC(), true
}

func shouldRefresh(now, fetchedAt time.Time) bool {
	if fetchedAt.IsZero() {
		return false
	}
	return now.Sub(fetchedAt) > staleAfter
}

func scheduleRefreshBestEffort(refreshKey string, fn func()) {
	log.Printf("[cache][refresh] schedule refreshKey=%q", refreshKey)
	_, _, _ = sf.Do("refresh:"+refreshKey, func() (interface{}, error) {
		log.Printf("[cache][refresh] running in background refreshKey=%q", refreshKey)
		go fn()
		return struct{}{}, nil
	})
}

// getTranslationsCached:
// 1) prova Redis (best-effort)
// 2) se Redis non disponibile o miss: prova S3 latest (best-effort)
// 3) se S3 non disponibile o miss: prova Tolgee (timeout 10s)
// 4) se Tolgee va ok: salva Redis + crea nuova versione su S3 + aggiorna latest (best-effort)
// 5) se tutto fallisce: ritorna JSON vuoto
func getTranslationsCached(ctx context.Context, s3c *s3Client, appID, lang string, nested bool) ([]byte, error) {
	mode := "flat"
	if nested {
		mode = "nested"
	}

	// Include la modalità nella cache per evitare collisioni tra output diversi
	redisKey := "translations:" + appID + ":" + lang + ":" + mode
	fetchedAtKey := redisKey + redisFetchedAtSuffix

	log.Printf("[cache][translations] request app=%q lang=%q mode=%q redisKey=%q", appID, lang, mode, redisKey)

	b, err := rdb.Get(ctx, redisKey).Bytes()
	if err == nil {
		log.Printf("[cache][redis] HIT key=%q bytes=%d", redisKey, len(b))
		if tsRaw, e2 := rdb.Get(ctx, fetchedAtKey).Result(); e2 == nil {
			if t, ok := parseUnixSeconds(tsRaw); ok {
				age := time.Since(t)
				log.Printf("[cache][redis] fetchedAt key=%q ts=%s age=%s", fetchedAtKey, t.Format(time.RFC3339), age)
				if shouldRefresh(time.Now().UTC(), t) {
					log.Printf("[cache][redis] STALE -> schedule refresh key=%q age=%s", redisKey, age)
					scheduleRefreshBestEffort(redisKey, func() {
						_, _ = getTranslationsCached(context.Background(), s3c, appID, lang, nested)
					})
				}
			} else {
				log.Printf("[cache][redis] fetchedAt parse failed key=%q raw=%q", fetchedAtKey, tsRaw)
			}
		} else {
			log.Printf("[cache][redis] fetchedAt missing/unavailable key=%q err=%v", fetchedAtKey, e2)
		}
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
			fallback, s3err := s3c.getLatest(ctx, appID, lang+"_"+mode)
			if s3err == nil && len(fallback) > 0 {
				_now := time.Now().UTC()
				log.Printf("[cache][s3] using fallback for translations app=%q lang=%q mode=%q -> write redis key=%q ttl=%s", appID, lang, mode, redisKey, redisValueTTL)
				_ = rdb.Set(ctx, redisKey, fallback, redisValueTTL).Err()
				_ = rdb.Set(ctx, fetchedAtKey, strconv.FormatInt(_now.Unix(), 10), redisValueTTL).Err()

				if createdAt, ok := s3c.headCreatedUTC(ctx, s3LatestKey(appID, lang+"_"+mode)); ok {
					age := _now.Sub(createdAt)
					log.Printf("[cache][s3] latest created_utc=%s age=%s key=%q", createdAt.Format(time.RFC3339), age, s3LatestKey(appID, lang+"_"+mode))
					if shouldRefresh(_now, createdAt) {
						log.Printf("[cache][s3] STALE -> schedule refresh key=%q age=%s", redisKey, age)
						scheduleRefreshBestEffort(redisKey, func() {
							_, _ = getTranslationsCached(context.Background(), s3c, appID, lang, nested)
						})
					}
				}

				return fallback, nil
			}
			if s3err != nil {
				log.Printf("[cache][s3] fallback failed for translations app=%q lang=%q mode=%q err=%v", appID, lang, mode, s3err)
			}
		} else {
			log.Printf("[cache][s3] disabled (no client) -> skip translations fallback")
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
		if tsRaw, e2 := rdb.Get(ctx, fetchedAtKey).Result(); e2 == nil {
			if t, ok := parseUnixSeconds(tsRaw); ok {
				age := time.Since(t)
				log.Printf("[cache][redis] fetchedAt key=%q ts=%s age=%s", fetchedAtKey, t.Format(time.RFC3339), age)
				if shouldRefresh(time.Now().UTC(), t) {
					log.Printf("[cache][redis] STALE -> schedule languages refresh key=%q age=%s", redisKey, age)
					scheduleRefreshBestEffort(redisKey, func() {
						_, _ = getLanguagesCached(context.Background(), s3c, appID)
					})
				}
			} else {
				log.Printf("[cache][redis] fetchedAt parse failed key=%q raw=%q", fetchedAtKey, tsRaw)
			}
		} else {
			log.Printf("[cache][redis] fetchedAt missing/unavailable key=%q err=%v", fetchedAtKey, e2)
		}
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

				if createdAt, ok := s3c.headCreatedUTC(ctx, s3LanguagesLatestKey(appID)); ok {
					age := _now.Sub(createdAt)
					log.Printf("[cache][s3] languages latest created_utc=%s age=%s key=%q", createdAt.Format(time.RFC3339), age, s3LanguagesLatestKey(appID))
					if shouldRefresh(_now, createdAt) {
						log.Printf("[cache][s3] STALE -> schedule languages refresh key=%q age=%s", redisKey, age)
						scheduleRefreshBestEffort(redisKey, func() {
							_, _ = getLanguagesCached(context.Background(), s3c, appID)
						})
					}
				}

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

	app.Get("/api/:app", func(c *fiber.Ctx) error {
		appID := c.Params("app")

		data, err := getLanguagesCached(c.Context(), s3c, appID)
		if err != nil {
			data = []byte("{}")
		}

		c.Set("Content-type", "application/json; charset=utf-8")
		return c.Status(200).Send(data)
	})
	app.Get("/api/:app/:lang", func(c *fiber.Ctx) error {
		appID := c.Params("app")
		lang := c.Params("lang")

		nested := false
		if raw := c.Query("nested", "false"); raw != "" {
			b, perr := strconv.ParseBool(raw)
			if perr == nil {
				nested = b
			}
		}

		data, err := getTranslationsCached(c.Context(), s3c, appID, lang, nested)
		if err != nil {
			data = []byte("{}")
		}

		c.Set("Content-type", "application/json; charset=utf-8")
		return c.Status(200).Send(data)
	})

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.Status(200).SendString("ok")
	})

	log.Fatal(app.Listen(":3000"))
}
