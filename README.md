# Illarion Gobaith Server on Selene

This is a Docker Compose setup for running Illarion in its Gobaith version on Selene.

## Prerequisites

- Docker
- Docker Compose

## Setup

1. Clone the repository
2. Initialize the bundle submodules with `git submodule update --init --recursive`
3. Run `docker compose up --build -d`

The Docker image build compiles `illarion-gobaith-ui` and includes the resulting
UI, all bundles, and the server configuration in the Selene runtime image.
