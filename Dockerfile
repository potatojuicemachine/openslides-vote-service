ARG CONTEXT=prod

FROM golang:1.26.2-alpine AS base

## Setup
ARG CONTEXT
WORKDIR /app/openslides-vote-service
ENV APP_CONTEXT=${CONTEXT}

## Install
RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

## External Information
EXPOSE 9013

## Command
HEALTHCHECK CMD ["/app/openslides-vote-service/openslides-vote-service", "health"]

# Development Image

FROM base AS dev

RUN ["go", "install", "github.com/githubnemo/CompileDaemon@latest"]

CMD CompileDaemon -log-prefix=false -build="go build" -command="./openslides-vote-service"

# Testing Image

FROM base AS tests

# Install Dockertest & Docker
RUN apk add --no-cache \
    build-base \
    docker && \
    go get -u github.com/ory/dockertest/v4 && \
    go install golang.org/x/lint/golint@latest && \
    chmod +x dev/container-tests.sh

## Command
STOPSIGNAL SIGKILL
CMD ["sleep", "inf"]

#base for use with local openslides-go
FROM base AS base-gowork
COPY ./lib ../lib
COPY ./vote.work ../go.work

#builder with local openslides-go
FROM base-gowork AS builder-gowork
RUN go build

FROM base AS builder
RUN go build

#prepare production image
FROM scratch AS pre-prod

## Setup
ARG CONTEXT
ENV APP_CONTEXT=prod

## External Information
LABEL org.opencontainers.image.title="OpenSlides Vote Service"
LABEL org.opencontainers.image.description="The OpenSlides Vote Service handles the votes for electronic polls."
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.source="https://github.com/OpenSlides/openslides-vote-service"

EXPOSE 9013

## Command
ENTRYPOINT ["/openslides-vote-service"]

HEALTHCHECK CMD ["/openslides-vote-service", "health"]

#finalize prod build with local openslides-go
FROM pre-prod AS prod-gowork

COPY --from=builder-gowork /app/openslides-vote-service/openslides-vote-service /

#finalize prod build
FROM pre-prod AS prod

COPY --from=builder /app/openslides-vote-service/openslides-vote-service /
