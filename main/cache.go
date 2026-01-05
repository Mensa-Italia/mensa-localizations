package main

import (
	"context"
	"errors"
	"log"
	localenv "mensalocalizations/tools/env"
)

func RebuildTheCache() {
	rootCtx := context.Background()
	appKey := localenv.GetTolgeeAppKey()

	_, bytesOfLanguages, err := GetLanguages(rootCtx, appKey)
	if err != nil || len(bytesOfLanguages) == 0 {
		return
	}

	_ = redisPut(rootCtx, "tolgee:languages", bytesOfLanguages, 0)

	var s3c *s3Client
	if localenv.GetS3Enabled() {
		c, err := newS3ClientFromEnv(rootCtx)
		if err != nil {
			log.Printf("[cache][s3] disabled (config error): %v", err)
		} else {
			log.Printf("[cache][s3] enabled bucket=%q", c.bucket)
			s3c = c
			_ = s3c.putObject(rootCtx, "tolgee:languages", bytesOfLanguages, "application/json", map[string]string{})
		}
	}

	langAndTrans, err := GetAllLanguagesAndTranslations(rootCtx, appKey, false)
	if err != nil {
		return
	}
	for name, translations := range langAndTrans {
		if len(translations) == 0 {
			continue
		}
		_ = redisPut(rootCtx, "tolgee:lang:"+name+":false", translations, 0)
		if s3c != nil {
			_ = s3c.putObject(rootCtx, "tolgee:lang:"+name+":false", translations, "application/json", map[string]string{})
		}
	}

	langAndTransNested, err := GetAllLanguagesAndTranslations(rootCtx, appKey, true)
	if err != nil {
		return
	}
	for name, translations := range langAndTransNested {
		if len(translations) == 0 {
			continue
		}
		_ = redisPut(rootCtx, "tolgee:lang:"+name+":true", translations, 0)
		if s3c != nil {
			_ = s3c.putObject(rootCtx, "tolgee:lang:"+name+":true", translations, "application/json", map[string]string{})
		}
	}

}

func GetLanguagesFromCache(ctx context.Context) ([]byte, error) {
	cached, err := redisGet(ctx, "tolgee:languages")
	if err == nil && len(cached) > 0 {
		return cached, nil
	}

	var s3c *s3Client
	if localenv.GetS3Enabled() {
		c, err := newS3ClientFromEnv(ctx)
		if err != nil {
			log.Printf("[cache][s3] disabled (config error): %v", err)
		} else {
			log.Printf("[cache][s3] enabled bucket=%q", c.bucket)
			s3c = c
			cached, err = s3c.getObject(ctx, "tolgee:languages")
			if err == nil && len(cached) > 0 {
				_ = redisPut(ctx, "tolgee:languages", cached, 0)
				return cached, nil
			}
		}
	}

	_, i, err := GetLanguages(ctx, localenv.GetTolgeeAppKey())
	if err != nil {
		return nil, err
	}

	_ = redisPut(ctx, "tolgee:languages", i, 0)
	if s3c != nil {
		_ = s3c.putObject(ctx, "tolgee:languages", i, "application/json", map[string]string{})
	}

	return i, nil
}

func GetTranslationsFromCache(ctx context.Context, lang string, nested bool) ([]byte, error) {
	nestedStr := "false"
	if nested {
		nestedStr = "true"
	}

	cached, err := redisGet(ctx, "tolgee:lang:"+lang+":"+nestedStr)
	if err == nil && len(cached) > 0 {
		return cached, nil
	}

	var s3c *s3Client
	if localenv.GetS3Enabled() {
		c, err := newS3ClientFromEnv(ctx)
		if err != nil {
			log.Printf("[cache][s3] disabled (config error): %v", err)
		} else {
			log.Printf("[cache][s3] enabled bucket=%q", c.bucket)
			s3c = c
			cached, err = s3c.getObject(ctx, "tolgee:lang:"+lang+":"+nestedStr)
			if err == nil && len(cached) > 0 {
				_ = redisPut(ctx, "tolgee:lang:"+lang, cached, 0)
				return cached, nil
			}
		}
	}

	if lang == "en" {
		return nil, errors.New("english translations not found in cache")
	}
	return GetTranslationsFromCache(ctx, "en", nested)
}
