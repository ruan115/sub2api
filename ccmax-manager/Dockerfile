FROM golang:1.26-bookworm AS build
WORKDIR /src
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/ccmax-manager .

FROM debian:bookworm-slim
RUN groupadd --system ccmax && useradd --system --gid ccmax ccmax
USER ccmax
COPY --from=build /out/ccmax-manager /usr/local/bin/ccmax-manager
ENV CCMAX_ADDR=0.0.0.0:8088
EXPOSE 8088
ENTRYPOINT ["ccmax-manager"]
