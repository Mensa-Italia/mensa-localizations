package main

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/goccy/go-json"
	"github.com/gofiber/fiber/v2"

	localenv "mensalocalizations/tools/env"
)

// --- Application entrypoint: single Tolgee app ---

func main() {
	appKey := localenv.GetTolgeeAppKey()
	if appKey == "" {
		log.Fatal("TOLGEE_APP_KEY is required")
	}

	if !fiber.IsChild() {
		RebuildTheCache()
	}

	app := fiber.New(fiber.Config{
		JSONEncoder: json.Marshal,
		JSONDecoder: json.Unmarshal,
	})

	app.Use(func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		duration := time.Since(start)
		c.Append("Server-Timing", "app;dur="+strconv.FormatInt(duration.Milliseconds(), 10)+"ms")
		return err
	})

	app.Get("/api/healthz", makeHealthHandler())
	app.All("/api/update", makeUpdateHandler())
	app.Get("/api/languages", makeLanguagesHandler())
	app.Get("/api/:lang", makeTranslationsHandler())

	// Catch-all 404: return inferred language (or en) payload
	app.All("*", makeFallbackHandler())

	log.Fatal(app.Listen(":3000"))
}

// --- Handlers ---

func makeHealthHandler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		return c.Status(http.StatusOK).SendString("ok")
	}
}

func makeUpdateHandler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		secret := localenv.GetWebhookSecret()
		header := c.Get("Tolgee-Signature")
		body := c.Body()
		if !verifyTolgeeSignature(secret, header, body) {
			log.Printf("[webhook] reject: invalid signature")
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid webhook signature"})
		}
		RebuildTheCache()
		return c.SendStatus(http.StatusOK)
	}
}

func makeLanguagesHandler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		cache, err := GetLanguagesFromCache(context.Background())
		if err != nil {
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.Status(http.StatusOK).Send(cache)
	}
}

func makeTranslationsHandler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		nested := c.Query("nested") == "true"
		lang := c.Params("lang")
		cache, err := GetTranslationsFromCache(context.Background(), lang, nested)
		if err != nil {
			return err
		}
		c.Set("Content-type", "application/json; charset=utf-8")
		return c.Status(http.StatusOK).Send(cache)
	}
}

func makeFallbackHandler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		nested := c.Query("nested") == "true"
		cache, err := GetTranslationsFromCache(context.Background(), "en", nested)
		if err != nil {
			return err
		}
		c.Set("Content-type", "application/json; charset=utf-8")
		return c.Status(http.StatusOK).Send(cache)
	}
}
