package main

import (
	"context"
	"log"
	localenv "mensalocalizations/tools/env"
)

func RebuildTheCache() {
	rootCtx := context.Background()
	appKey := localenv.GetTolgeeAppKey()

	_, bytesOfLanguages, err := GetLanguages(rootCtx, appKey)
	if err != nil {
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
		_ = redisPut(rootCtx, "tolgee:lang:"+name, translations, 0)
		if s3c != nil {
			_ = s3c.putObject(rootCtx, "tolgee:lang:"+name, translations, "application/json", map[string]string{})
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

func GetTranslationsFromCache(ctx context.Context, lang string) ([]byte, error) {
	cached, err := redisGet(ctx, "tolgee:lang:"+lang)
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
			cached, err = s3c.getObject(ctx, "tolgee:lang:"+lang)
			if err == nil && len(cached) > 0 {
				_ = redisPut(ctx, "tolgee:lang:"+lang, cached, 0)
				return cached, nil
			}
		}
	}

	i, err := GetTranslations(ctx, localenv.GetTolgeeAppKey(), lang, false)
	if err != nil {
		i2, err := GetTranslations(ctx, localenv.GetTolgeeAppKey(), "en", false)
		if err != nil {
			return nil, err
		}
		return i2["en"], nil
	}

	_ = redisPut(ctx, "tolgee:lang:"+lang, i[lang], 0)
	if s3c != nil {
		_ = s3c.putObject(ctx, "tolgee:lang:"+lang, i[lang], "application/json", map[string]string{})
	}

	return i[lang], nil
}
