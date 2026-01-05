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
	"net"
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
		Addr: localenv.GetRedisAddr(),
		DB:   0,
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

	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()

	b, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, err
	}
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

func s3LatestKey(appID, lang string) string {
	return fmt.Sprintf("localizations/%s/%s/latest.json", sanitizeKeyPart(appID), sanitizeKeyPart(lang))
}

func s3VersionKey(appID, lang, tsUTC, sha string) string {
	return fmt.Sprintf("localizations/%s/%s/%s_%s.json", sanitizeKeyPart(appID), sanitizeKeyPart(lang), tsUTC, sha)
}

func sanitizeKeyPart(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "..", "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "\\", "-")
	if s == "" {
		return "_"
	}
	return s
}

func isTimeoutOrTemporary(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout() || ne.Temporary()
	}
	return errors.Is(err, context.DeadlineExceeded)
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

	// 1) Cache hit Redis
	b, err := rdb.Get(ctx, redisKey).Bytes()
	if err == nil {
		return b, nil
	}
	// Se Redis è offline (o altro errore non-Nil), non fallire: continuiamo con i fallback.
	if err != nil && err != redis.Nil {
		log.Printf("redis get failed (best-effort): %v", err)
	}

	// 2) Cache miss (o Redis non disponibile): singleflight per evitare N chiamate simultanee
	v, sfErr, _ := sf.Do(redisKey, func() (interface{}, error) {
		// Ricontrollo Redis dentro singleflight
		bb, e := rdb.Get(ctx, redisKey).Bytes()
		if e == nil {
			return bb, nil
		}
		if e != nil && e != redis.Nil {
			log.Printf("redis get failed (best-effort, singleflight): %v", e)
		}

		// Prova S3 latest come cache (best-effort)
		if s3c != nil {
			fallback, s3err := s3c.getLatest(ctx, appID, lang+"_"+mode)
			if s3err == nil && len(fallback) > 0 {
				// Riempi Redis best-effort per la prossima volta
				ttl := 10 * time.Minute
				_ = rdb.Set(ctx, redisKey, fallback, ttl).Err()
				return fallback, nil
			}
			if s3err != nil {
				log.Printf("s3 getLatest failed (best-effort): %v", s3err)
			}
		}

		// Ultima risorsa: Tolgee (timeout 10s)
		tolgeeURL := "https://app.tolgee.io/v2/projects/export"
		tolgeeAK := appID
		if tolgeeAK == "" {
			// Input invalido: qui preferisco mantenere un errore, ma l'handler lo tradurrà comunque in {}.
			return []byte("{}"), nil
		}

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

		// Se nested=true: niente structureDelimiter (Tolgee ritorna struttura annidata).
		// Se nested=false: forziamo JSON piatto con delimiter (compat con comportamento precedente).
		if !nested {
			req.SetQueryParam("structureDelimiter", "")
		}

		resp, e := req.Get(tolgeeURL)

		if e != nil {
			log.Printf("tolgee request failed (best-effort): %v", e)
			return []byte("{}"), nil
		}

		if resp.StatusCode() < http.StatusOK || resp.StatusCode() >= http.StatusMultipleChoices {
			log.Printf("tolgee non-2xx status (best-effort): %d", resp.StatusCode())
			return []byte("{}"), nil
		}

		body := resp.Body()
		if len(body) == 0 {
			log.Printf("tolgee returned empty body (best-effort)")
			return []byte("{}"), nil
		}

		// Salva Redis con TTL (best-effort)
		ttl := 10 * time.Minute
		_ = rdb.Set(ctx, redisKey, body, ttl).Err()

		// Salva su S3 (version + latest) (best-effort)
		if s3c != nil {
			_ = s3c.putVersionAndLatest(ctx, appID, lang+"_"+mode, body)
		}

		return body, nil
	})
	if sfErr != nil {
		// Per sicurezza: in teoria non dovrebbe più succedere, ma manteniamo una risposta stabile.
		log.Printf("singleflight returned error (best-effort): %v", sfErr)
		return []byte("{}"), nil
	}

	bb, ok := v.([]byte)
	if !ok {
		return []byte("{}"), nil
	}
	return bb, nil
}

func main() {
	rootCtx := context.Background()
	var s3c *s3Client
	if localenv.GetS3Enabled() {
		c, err := newS3ClientFromEnv(rootCtx)
		if err != nil {
			log.Printf("S3 disabled (config error): %v", err)
		} else {
			s3c = c
		}
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
		c.Append("Server-Timing", "app;dur="+duration.String())
		return err
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
