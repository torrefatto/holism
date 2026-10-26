FROM golang:1.24 AS build
WORKDIR /src
COPY go.work ./
COPY api/ ./api/
COPY drone/ ./drone/
RUN CGO_ENABLED=0 go build -o /out/drone ./drone/cmd/drone

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/drone /drone
ENTRYPOINT ["/drone"]
