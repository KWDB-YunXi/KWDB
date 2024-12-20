// Copyright 2019 The Cockroach Authors.
// Copyright (c) 2022-present, Shanghai Yunxi Technology Co, Ltd. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// This software (KWDB) is licensed under Mulan PSL v2.
// You can use this software according to the terms and conditions of the Mulan PSL v2.
// You may obtain a copy of Mulan PSL v2 at:
//          http://license.coscl.org.cn/MulanPSL2
// THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, WITHOUT WARRANTIES OF ANY KIND,
// EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO NON-INFRINGEMENT,
// MERCHANTABILITY OR FIT FOR A PARTICULAR PURPOSE.
// See the Mulan PSL v2 for more details.

package cloud

import (
	"context"
	"io"
	"net/url"
	"path"
	"strings"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/util/contextutil"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/cockroachdb/errors"
)

type s3Storage struct {
	bucket   *string
	conf     *roachpb.ExternalStorage_S3
	prefix   string
	s3       *s3.S3
	settings *cluster.Settings
}

var _ ExternalStorage = &s3Storage{}

func s3QueryParams(conf *roachpb.ExternalStorage_S3) string {
	q := make(url.Values)
	setIf := func(key, value string) {
		if value != "" {
			q.Set(key, value)
		}
	}
	setIf(S3AccessKeyParam, conf.AccessKey)
	setIf(S3SecretParam, conf.Secret)
	setIf(S3TempTokenParam, conf.TempToken)
	setIf(S3EndpointParam, conf.Endpoint)
	setIf(S3RegionParam, conf.Region)
	setIf(AuthParam, conf.Auth)

	return q.Encode()
}

func makeS3Storage(
	ctx context.Context,
	ioConf base.ExternalIOConfig,
	conf *roachpb.ExternalStorage_S3,
	settings *cluster.Settings,
) (ExternalStorage, error) {
	if conf == nil {
		return nil, errors.Errorf("s3 upload requested but info missing")
	}
	region := conf.Region
	config := conf.Keys()
	if conf.Endpoint != "" {
		if ioConf.DisableHTTP {
			return nil, errors.New(
				"custom endpoints disallowed for s3 due to --external-io-disable-http flag")
		}
		config.Endpoint = &conf.Endpoint
		if conf.Region == "" {
			region = "default-region"
		}
		client, err := makeHTTPClient(settings)
		if err != nil {
			return nil, err
		}
		config.HTTPClient = client
	}

	// "specified": use credentials provided in URI params; error if not present.
	// "implicit": enable SharedConfig, which loads in credentials from environment.
	//             Detailed in https://docs.aws.amazon.com/sdk-for-go/api/aws/session/
	// "": default to `specified`.
	opts := session.Options{}
	switch conf.Auth {
	case "", authParamSpecified:
		if conf.AccessKey == "" {
			return nil, errors.Errorf(
				"%s is set to '%s', but %s is not set",
				AuthParam,
				authParamSpecified,
				S3AccessKeyParam,
			)
		}
		if conf.Secret == "" {
			return nil, errors.Errorf(
				"%s is set to '%s', but %s is not set",
				AuthParam,
				authParamSpecified,
				S3SecretParam,
			)
		}
		opts.Config.MergeIn(config)
	case authParamImplicit:
		if ioConf.DisableImplicitCredentials {
			return nil, errors.New(
				"implicit credentials disallowed for s3 due to --external-io-implicit-credentials flag")
		}
		opts.SharedConfigState = session.SharedConfigEnable
	default:
		return nil, errors.Errorf("unsupported value %s for %s", conf.Auth, AuthParam)
	}

	sess, err := session.NewSessionWithOptions(opts)
	if err != nil {
		return nil, errors.Wrap(err, "new aws session")
	}
	if region == "" {
		err = delayedRetry(ctx, func() error {
			var err error
			region, err = s3manager.GetBucketRegion(ctx, sess, conf.Bucket, "us-east-1")
			return err
		})
		if err != nil {
			return nil, errors.Wrap(err, "could not find s3 bucket's region")
		}
	}
	sess.Config.Region = aws.String(region)
	if conf.Endpoint != "" {
		sess.Config.S3ForcePathStyle = aws.Bool(true)
	}
	if log.V(2) {
		sess.Config.LogLevel = aws.LogLevel(aws.LogDebugWithRequestRetries | aws.LogDebugWithRequestErrors)
	}
	maxRetries := 10
	sess.Config.MaxRetries = &maxRetries

	return &s3Storage{
		bucket:   aws.String(conf.Bucket),
		conf:     conf,
		prefix:   conf.Prefix,
		s3:       s3.New(sess),
		settings: settings,
	}, nil
}

func (s *s3Storage) Conf() roachpb.ExternalStorage {
	return roachpb.ExternalStorage{
		Provider: roachpb.ExternalStorageProvider_S3,
		S3Config: s.conf,
	}
}

func (s *s3Storage) WriteFile(ctx context.Context, basename string, content io.ReadSeeker) error {
	err := contextutil.RunWithTimeout(ctx, "put s3 object",
		timeoutSetting.Get(&s.settings.SV),
		func(ctx context.Context) error {
			_, err := s.s3.PutObjectWithContext(ctx, &s3.PutObjectInput{
				Bucket: s.bucket,
				Key:    aws.String(path.Join(s.prefix, basename)),
				Body:   content,
			})
			return err
		})
	return errors.Wrap(err, "failed to put s3 object")
}

func (s *s3Storage) AppendWrite(ctx context.Context, file string, content io.ReadSeeker) error {
	return errors.New("s3 does not support additional writing")
}

func (s *s3Storage) ReadFile(ctx context.Context, basename string) (io.ReadCloser, error) {
	// https://gitee.com/kwbasedb/kwbase/issues/23859
	out, err := s.s3.GetObjectWithContext(ctx, &s3.GetObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(path.Join(s.prefix, basename)),
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to get s3 object")
	}
	return out.Body, nil
}

func (s *s3Storage) Seek(
	ctx context.Context, filename string, offset int64, endOffset int64, whence int,
) (io.ReadCloser, error) {
	return nil, errors.New("s3Storage does not support seek")
}

func getPrefixBeforeWildcard(p string) string {
	globIndex := strings.IndexAny(p, "*?[")
	if globIndex < 0 {
		return p
	}
	return path.Dir(p[:globIndex])
}

func (s *s3Storage) ListFiles(ctx context.Context, patternSuffix string) ([]string, error) {
	var fileList []string

	pattern := s.prefix
	if patternSuffix != "" {
		if containsGlob(s.prefix) {
			return nil, errors.New("prefix cannot contain globs pattern when passing an explicit pattern")
		}
		pattern = path.Join(pattern, patternSuffix)
	}

	var matchErr error
	err := s.s3.ListObjectsPagesWithContext(
		ctx,
		&s3.ListObjectsInput{
			Bucket: s.bucket,
			Prefix: aws.String(getPrefixBeforeWildcard(s.prefix)),
		},
		func(page *s3.ListObjectsOutput, lastPage bool) bool {
			for _, fileObject := range page.Contents {
				matches, err := path.Match(pattern, *fileObject.Key)
				if err != nil {
					matchErr = err
					return false
				}
				if matches {
					if patternSuffix != "" {
						if !strings.HasPrefix(*fileObject.Key, s.prefix) {
							// TODO(dt): return a nice rel-path instead of erroring out.
							matchErr = errors.New("pattern matched file outside of path")
							return false
						}
						fileList = append(fileList, strings.TrimPrefix(strings.TrimPrefix(*fileObject.Key, s.prefix), "/"))
					} else {
						s3URL := url.URL{
							Scheme:   "s3",
							Host:     *s.bucket,
							Path:     *fileObject.Key,
							RawQuery: s3QueryParams(s.conf),
						}
						fileList = append(fileList, s3URL.String())
					}
				}
			}
			return !lastPage
		},
	)
	if err != nil {
		return nil, errors.Wrap(err, `failed to list s3 bucket`)
	}
	if matchErr != nil {
		return nil, errors.Wrap(matchErr, `failed to list s3 bucket`)
	}

	return fileList, nil
}

func (s *s3Storage) Delete(ctx context.Context, basename string) error {
	return contextutil.RunWithTimeout(ctx, "delete s3 object",
		timeoutSetting.Get(&s.settings.SV),
		func(ctx context.Context) error {
			_, err := s.s3.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
				Bucket: s.bucket,
				Key:    aws.String(path.Join(s.prefix, basename)),
			})
			return err
		})
}

func (s *s3Storage) Size(ctx context.Context, basename string) (int64, error) {
	var out *s3.HeadObjectOutput
	err := contextutil.RunWithTimeout(ctx, "get s3 object header",
		timeoutSetting.Get(&s.settings.SV),
		func(ctx context.Context) error {
			var err error
			out, err = s.s3.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
				Bucket: s.bucket,
				Key:    aws.String(path.Join(s.prefix, basename)),
			})
			return err
		})
	if err != nil {
		return 0, errors.Wrap(err, "failed to get s3 object headers")
	}
	return *out.ContentLength, nil
}

func (s *s3Storage) Close() error {
	return nil
}
