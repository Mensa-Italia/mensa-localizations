package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	localenv "mensalocalizations/tools/env"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var ErrS3ClientNil = errors.New("s3 client is nil")

// S3 client wrapper
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

// getObject reads a raw object by key from the configured bucket.
func (s *s3Client) getObject(ctx context.Context, key string) ([]byte, error) {
	if s == nil {
		return nil, ErrS3ClientNil
	}
	log.Printf("[s3] GET key=%q bucket=%q", key, s.bucket)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("[s3] GET error key=%q err=%v", key, err)
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()

	b, err := io.ReadAll(out.Body)
	if err != nil {
		log.Printf("[s3] read error key=%q err=%v", key, err)
		return nil, err
	}
	log.Printf("[s3] GET ok key=%q bytes=%d", key, len(b))
	return b, nil
}

// putObject writes a raw object by key into the configured bucket.
// If contentType is empty, application/octet-stream is used.
// Metadata can be nil.
func (s *s3Client) putObject(ctx context.Context, key string, payload []byte, contentType string, metadata map[string]string) error {
	if s == nil {
		return ErrS3ClientNil
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	log.Printf("[s3] PUT key=%q bucket=%q bytes=%d", key, s.bucket, len(payload))
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(payload),
		ContentType: aws.String(contentType),
		Metadata:    metadata,
		ACL:         types.ObjectCannedACLPrivate,
	})
	if err != nil {
		log.Printf("[s3] PUT error key=%q err=%v", key, err)
		return err
	}
	return nil
}
