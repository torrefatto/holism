FROM golang:1.24 AS build
# The cli module is self-contained (no dependency on api/mind/drone), so we
# build it standalone without go.work — copying go.work here would fail
# because it references module directories that are not in the build context.
WORKDIR /src
COPY cli/ ./
RUN CGO_ENABLED=0 go build -o /out/holism ./cmd/holism

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/holism /holism
ENTRYPOINT ["/holism"]
