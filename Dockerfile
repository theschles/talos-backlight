FROM golang:1.26.11 AS builder
WORKDIR /src
COPY controller/go.mod controller/go.sum ./
RUN go mod download
COPY controller/*.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /backlight .

FROM scratch
COPY --from=builder --chmod=0555 /backlight /backlight
ENTRYPOINT ["/backlight"]
CMD ["serve"]
