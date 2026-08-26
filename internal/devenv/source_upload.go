package devenv

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type SourceUploader interface {
	Create(context.Context, Identity) (SourceUpload, error)
}

type S3PresignAPI interface {
	PresignPutObject(context.Context, *s3.PutObjectInput,
		...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
	PresignGetObject(context.Context, *s3.GetObjectInput,
		...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

type SourceDownloader interface {
	DownloadURL(context.Context, Identity, string) (string, error)
}

type S3SourceUploader struct {
	Client S3PresignAPI
	Bucket string
	Now    func() time.Time
}

func (u S3SourceUploader) Create(ctx context.Context, identity Identity) (SourceUpload, error) {
	if u.Client == nil || strings.TrimSpace(u.Bucket) == "" {
		return SourceUpload{}, errors.New("source uploader is not configured")
	}
	uploadID, err := newID("upload")
	if err != nil {
		return SourceUpload{}, err
	}
	ownerPart := strings.TrimPrefix(identity.PrincipalID, "owner:v1:")
	if ownerPart == "" || ownerPart == identity.PrincipalID {
		return SourceUpload{}, errors.New("source uploads require an interactive owner")
	}
	key := "environments/uploads/" + ownerPart + "/" + uploadID + "/source.tgz"
	expires := time.Hour
	presigned, err := u.Client.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(u.Bucket), Key: aws.String(key), ContentType: aws.String("application/gzip"),
		Metadata: map[string]string{"owner-key": identity.PrincipalID},
	}, func(options *s3.PresignOptions) {
		options.Expires = expires
	})
	if err != nil {
		return SourceUpload{}, err
	}
	now := time.Now
	if u.Now != nil {
		now = u.Now
	}
	return SourceUpload{
		BundleKey: key, URL: presigned.URL,
		Headers: map[string]string{
			"content-type": "application/gzip", "x-amz-meta-owner-key": identity.PrincipalID,
		},
		ExpiresAt: now().UTC().Add(expires),
	}, nil
}

func (u S3SourceUploader) DownloadURL(ctx context.Context, identity Identity, key string) (string, error) {
	if u.Client == nil || strings.TrimSpace(u.Bucket) == "" {
		return "", errors.New("source downloader is not configured")
	}
	if err := validateOwnedBundleKey(identity, key); err != nil {
		return "", err
	}
	presigned, err := u.Client.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(u.Bucket), Key: aws.String(key),
	}, func(options *s3.PresignOptions) {
		options.Expires = 15 * time.Minute
	})
	if err != nil {
		return "", err
	}
	return presigned.URL, nil
}

func validateOwnedBundleKey(identity Identity, key string) error {
	ownerPart := strings.TrimPrefix(identity.PrincipalID, "owner:v1:")
	prefix := "environments/uploads/" + ownerPart + "/"
	if ownerPart == "" || ownerPart == identity.PrincipalID || !strings.HasPrefix(key, prefix) ||
		!strings.HasSuffix(key, "/source.tgz") || strings.Contains(key, "..") ||
		strings.ContainsAny(key, "\r\n\\") {
		return errors.New("source bundle is not owned by the authenticated user")
	}
	return nil
}
