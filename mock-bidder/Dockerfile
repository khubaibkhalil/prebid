FROM golang:1.22-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -o mock-bidder ./

FROM gcr.io/distroless/base-debian12:nonroot
WORKDIR /
COPY --from=build /app/mock-bidder /mock-bidder
EXPOSE 8081
ENTRYPOINT ["/mock-bidder"]
