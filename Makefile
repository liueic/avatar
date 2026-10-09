# Convenience targets. macOS note: builds pass SDKROOT explicitly because the
# onnxruntime cgo shim can hit a linker/tbd mismatch with the newest SDK.
SDKROOT_DARWIN := $(shell [ -d /Library/Developer/CommandLineTools/SDKs/MacOSX26.5.sdk ] && echo /Library/Developer/CommandLineTools/SDKs/MacOSX26.5.sdk)

export SDKROOT := $(if $(shell uname | grep -i darwin),$(SDKROOT_DARWIN),)

.PHONY: build test vet tidy run docker clean model ort

build:
	go build -o bin/avater ./cmd/avater

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

# Local run with the trivial moderation engine (no model needed)
run: build
	AVATER_ADMIN_TOKEN=dev-token \
	AVATER_MODERATION_ENGINE=none \
	AVATER_MODERATION_NONE_POLICY=approve \
	./bin/avater

docker:
	docker build -t avater .

# Fetch the moderation model (verifies its pinned SHA-256)
model:
	mkdir -p models
	curl -fsSL -o models/image-safety-classifier-xs.onnx \
	  https://huggingface.co/OwenElliott/image-safety-classifier-xs/resolve/main/onnx/image-safety-classifier-xs.onnx
	echo "8c28c49d9075f3ad15ebdc2961f02d5b3f99be944815b848b49c9f0e6f3fb689  models/image-safety-classifier-xs.onnx" | shasum -a 256 -c

# Fetch a local onnxruntime shared library for development (macOS arm64 shown)
ORT_VERSION ?= 1.31.0
ort:
	mkdir -p third_party/onnxruntime
	curl -fsSL -o /tmp/ort.tgz \
	  https://github.com/microsoft/onnxruntime/releases/download/v$(ORT_VERSION)/onnxruntime-osx-arm64-$(ORT_VERSION).tgz
	tar -xzf /tmp/ort.tgz -C third_party/onnxruntime --strip-components=1

clean:
	rm -rf bin
