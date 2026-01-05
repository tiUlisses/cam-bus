// internal/storage/minio_store.go
package storage

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type ImageStore interface {
	SaveSnapshot(ctx context.Context, key string, data []byte, contentType string) (string, error)
}

type MinioStore struct {
	client  *minio.Client
	bucket  string
	prefix  string
	baseURL *url.URL
	useSSL  bool
}

// Global simples pra driver usar sem ter que passar dependência em tudo
var DefaultStore ImageStore

func NewMinioStoreFromEnv() (*MinioStore, error) {
	endpoint := getenv("MINIO_ENDPOINT", "localhost:9000")
	accessKey := os.Getenv("MINIO_ACCESS_KEY")
	secretKey := os.Getenv("MINIO_SECRET_KEY")
	bucket := getenv("MINIO_BUCKET", "rtls-snapshots")
	prefix := getenv("MINIO_PREFIX", "")
	useSSL := getenv("MINIO_USE_SSL", "false") == "true"
	base := getenv("MINIO_PUBLIC_BASE_URL", "")
	publicRead := getenv("MINIO_PUBLIC_READ", "false") == "true"

	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("MINIO_ACCESS_KEY / MINIO_SECRET_KEY não configurados")
	}

	cli, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("erro criando cliente MinIO: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Cria bucket se não existir
	err = cli.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
	if err != nil {
		exists, errBucketExists := cli.BucketExists(ctx, bucket)
		if errBucketExists != nil || !exists {
			return nil, fmt.Errorf("erro criando/verificando bucket %s: %w", bucket, err)
		}
	}

	if publicRead {
		resource := fmt.Sprintf("arn:aws:s3:::%s/*", bucket)
		cleanPrefix := strings.Trim(prefix, "/")
		if cleanPrefix != "" {
			resource = fmt.Sprintf("arn:aws:s3:::%s/%s/*", bucket, cleanPrefix)
		}
		policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["%s"]}]}`, resource)
		if err := cli.SetBucketPolicy(ctx, bucket, policy); err != nil {
			return nil, fmt.Errorf("erro configurando policy pública no bucket %s: %w", bucket, err)
		}
	}

	var u *url.URL
	if base != "" {
		u, err = url.Parse(base)
		if err != nil {
			return nil, fmt.Errorf("MINIO_PUBLIC_BASE_URL inválida: %w", err)
		}
	}

	log.Printf("[minio] conectado ao endpoint %s, bucket=%s", endpoint, bucket)

	return &MinioStore{
		client:  cli,
		bucket:  bucket,
		prefix:  strings.Trim(prefix, "/"),
		baseURL: u,
		useSSL:  useSSL,
	}, nil
}

func (s *MinioStore) SaveSnapshot(ctx context.Context, key string, data []byte, contentType string) (string, error) {
	if contentType == "" {
		contentType = "image/jpeg"
	}

	reader := bytes.NewReader(data)

	objectKey := joinObjectKey(s.prefix, key)

	_, err := s.client.PutObject(
		ctx,
		s.bucket,
		objectKey,
		reader,
		int64(len(data)),
		minio.PutObjectOptions{
			ContentType: contentType,
		},
	)
	if err != nil {
		return "", fmt.Errorf("erro ao enviar objeto pro MinIO: %w", err)
	}

	// Se for configurado um baseURL público, usamos ele
	if s.baseURL != nil {
		u := *s.baseURL
		if u.Path == "" || u.Path == "/" {
			u.Path = "/" + objectKey
		} else {
			u.Path = fmt.Sprintf("%s/%s", trimSuffix(u.Path, "/"), objectKey)
		}
		return u.String(), nil
	}

	// Fallback: URL bruta do endpoint S3
	scheme := "http"
	if s.useSSL {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/%s/%s", scheme, s.client.EndpointURL().Host, s.bucket, objectKey), nil
}

func getenv(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}

func trimSuffix(s, suffix string) string {
	if len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)]
	}
	return s
}

func joinObjectKey(prefix, key string) string {
	cleanPrefix := strings.Trim(prefix, "/")
	cleanKey := strings.TrimPrefix(key, "/")
	if cleanPrefix == "" {
		return cleanKey
	}
	if cleanKey == "" {
		return cleanPrefix
	}
	return cleanPrefix + "/" + cleanKey
}
