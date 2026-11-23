# The build stage's base image for the e2e services: golang + this checkout's runtime at /runtime.
#
# The projects under examples/ build against the runtime in this repository (go.mod: replace
# github.com/MIERAI/api-factory/runtime => ../../../runtime, which from the generated Dockerfile's WORKDIR /src is
# /runtime). The generated Dockerfiles stay as they are: compose swaps their `FROM …/golang:<go>-alpine` for this image
# (build.additional_contexts, a BuildKit named context), so the images are built by the very Dockerfile a project
# outside this repository gets, against the runtime of this commit (not the last release). Context: the repository root.
ARG IMAGE_MIRROR=mirror.gcr.io/library
ARG GO_VERSION
FROM ${IMAGE_MIRROR}/golang:${GO_VERSION}-alpine
COPY runtime /runtime
