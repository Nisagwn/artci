# Tüm servisler tek imajda; hangisinin çalışacağını compose'daki command seçer.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# distroless/static: CA sertifikaları var, kabuk yok, root olmayan kullanıcı.
# Saat dilimi verisi ikili dosyalara gömülü (time/tzdata).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /app/
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/app/api"]
