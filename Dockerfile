FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY . .

# Declared after the source copy so a new stamp only re-links the binary.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ENV STAMP="-s -w \
 -X github.com/NullGeorge/lastseen/internal/version.Version=${VERSION} \
 -X github.com/NullGeorge/lastseen/internal/version.Commit=${COMMIT} \
 -X github.com/NullGeorge/lastseen/internal/version.Date=${DATE}"

RUN CGO_ENABLED=0 go build -trimpath -ldflags="$STAMP" -o /out/lastseen .

FROM scratch AS export
COPY --from=build /out/lastseen /

FROM alpine:3.22
RUN adduser -S -D -H lastseen
COPY --from=build /out/lastseen /usr/local/bin/lastseen
USER lastseen
EXPOSE 9000
ENTRYPOINT ["/usr/local/bin/lastseen"]
