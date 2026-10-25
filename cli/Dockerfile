FROM golang:1.24 AS build
WORKDIR /src
COPY go.work ./
COPY cli/ ./cli/
RUN CGO_ENABLED=0 go build -o /out/holism ./cli/cmd/holism

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/holism /holism
ENTRYPOINT ["/holism"]
