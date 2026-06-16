
golangci_lint_version := "v2.12.2"
goreleaser_version := "v2.13.3"

@_:
  just --list

# Run all tests
test:
    go test ./...

# Run golangci-lint with --fix
fix:
    go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@{{golangci_lint_version}} run --fix

# Run golangci-lint without --fix
lint:
    go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@{{golangci_lint_version}} run

# Build the binary
build:
    go build -o kwinl .

# Install the binary
install:
    go install .

# Remove build artifacts
clean:
    rm -rf kwinl kwinl.exe dist/

# Build with version info (local dev)
build-dev:
    go build -ldflags "-X main.version=dev -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o kwinl .

# Test goreleaser locally
release-dry-run:
    go run github.com/goreleaser/goreleaser/v2@{{goreleaser_version}} release --snapshot --clean

# Pre-release safety checks
_release-check:
    #!/usr/bin/env sh
    set -e
    if [ -n "$(git status --porcelain)" ]; then
        echo "Error: uncommitted changes. Commit or stash first." >&2
        exit 1
    fi
    branch=$(git branch --show-current)
    if [ "$branch" != "master" ]; then
        echo "Error: not on master branch (on $branch)" >&2
        exit 1
    fi
    git fetch origin master --tags
    local_head=$(git rev-parse HEAD)
    remote_head=$(git rev-parse origin/master)
    if [ "$local_head" != "$remote_head" ]; then
        echo "Error: local master differs from origin/master. Pull or push first." >&2
        exit 1
    fi
    latest_tag=$(git describe --tags --abbrev=0 2>/dev/null || echo "")
    if [ -n "$latest_tag" ]; then
        tag_commit=$(git rev-parse "$latest_tag"^{})
        if [ "$local_head" = "$tag_commit" ]; then
            echo "Error: HEAD is already tagged as $latest_tag. Make new commits first." >&2
            exit 1
        fi
    fi

# Release a new patch version (eg. v1.0.0 -> v1.0.1)
release-patch: _release-check
    #!/usr/bin/env sh
    set -e
    latest=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
    major=$(echo "$latest" | sed 's/v//' | cut -d. -f1)
    minor=$(echo "$latest" | sed 's/v//' | cut -d. -f2)
    patch=$(echo "$latest" | sed 's/v//' | cut -d. -f3)
    new="v${major}.${minor}.$((patch + 1))"
    echo "Releasing $new (was $latest)"
    git tag "$new"
    git push origin "$new"

# Release a new minor version (eg. v1.0.0 -> v1.1.0)
release-minor: _release-check
    #!/usr/bin/env sh
    set -e
    latest=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
    major=$(echo "$latest" | sed 's/v//' | cut -d. -f1)
    minor=$(echo "$latest" | sed 's/v//' | cut -d. -f2)
    new="v${major}.$((minor + 1)).0"
    echo "Releasing $new (was $latest)"
    git tag "$new"
    git push origin "$new"

# Release a new major version (eg. v1.0.0 -> v2.0.0)
release-major: _release-check
    #!/usr/bin/env sh
    set -e
    latest=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
    major=$(echo "$latest" | sed 's/v//' | cut -d. -f1)
    new="v$((major + 1)).0.0"
    echo "Releasing $new (was $latest)"
    git tag "$new"
    git push origin "$new"

alias release := release-patch
