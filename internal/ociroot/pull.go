package ociroot

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// PullImage resolves ref and fetches the linux/amd64 image from a registry.
// Credentials come from the standard docker config keychain
// (authn.DefaultKeychain), which reads $DOCKER_CONFIG/config.json: this covers
// private registries and lifts the anonymous pull rate limits that public
// registries (Docker Hub, public ECR) impose per source IP. DefaultKeychain
// resolves static base64 auths natively without pulling in docker/cli; only
// external credential helpers (credsStore/credHelpers) would need extra
// binaries, and those are not required for a mounted static config.json. With
// no config present it falls back to anonymous, preserving public-image pulls.
func PullImage(ctx context.Context, ref string) (v1.Image, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("ociroot: parse image ref %q: %w", ref, err)
	}

	img, err := remote.Image(parsed,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}),
	)
	if err != nil {
		return nil, fmt.Errorf("ociroot: pull image %q: %w", ref, err)
	}
	return img, nil
}
