FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /mediapolicy ./cmd/mediapolicy

FROM alpine:3
RUN apk add --no-cache ca-certificates ffmpeg
COPY --from=build /mediapolicy /usr/local/bin/mediapolicy
USER 1000:1000
ENTRYPOINT ["mediapolicy"]
CMD ["-config", "/config/policy.json", "-state-dir", "/state", "-listen", ":8792"]
