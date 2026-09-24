# Toolchain image for scripts/dev.sh and scripts/deploy.sh: the exact Go
# version from go.mod, plus Node (owner-UI JS harness tests) and the
# PostgreSQL client (applying migrations). Nothing project-specific is
# baked in; the repository is mounted at /src.
FROM golang:1.25
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends nodejs postgresql-client \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
