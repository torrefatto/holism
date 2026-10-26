FROM golang:1.24 AS build
WORKDIR /src
COPY go.work ./
COPY api/ ./api/
COPY mind/ ./mind/
RUN CGO_ENABLED=0 go build -o /out/mind ./mind/cmd/mind

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/mind /mind
ENTRYPOINT ["/mind"]
