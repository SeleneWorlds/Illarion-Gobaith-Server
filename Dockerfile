# syntax=docker/dockerfile:1

FROM node:22-alpine AS ui-build-base

RUN apk add --no-cache git \
    && corepack enable


FROM ui-build-base AS ui-build

COPY moonlight/moonlight-ui/ /src/moonlight/moonlight-ui/
WORKDIR /src/illarion-gobaith-ui
COPY bundles/illarion-gobaith-ui/package.json bundles/illarion-gobaith-ui/pnpm-lock.yaml bundles/illarion-gobaith-ui/pnpm-workspace.yaml ./
COPY bundles/illarion-gobaith-ui/illarion-client-core/package.json ./illarion-client-core/package.json
COPY bundles/illarion-gobaith-ui/illarion-gobaith-ui/package.json ./illarion-gobaith-ui/package.json
# Shared sources in /src/moonlight/moonlight-ui resolve dependencies through /src/node_modules.
RUN pnpm install --frozen-lockfile \
    && ln -s /src/illarion-gobaith-ui/node_modules /src/node_modules

COPY bundles/illarion-gobaith-ui/ ./
RUN pnpm run build


FROM ui-build-base AS admin-ui-build

COPY moonlight/moonlight-ui/ /src/moonlight-ui/
WORKDIR /src/moonlight-admin
COPY moonlight/moonlight-admin/package.json moonlight/moonlight-admin/pnpm-lock.yaml moonlight/moonlight-admin/pnpm-workspace.yaml ./
# Shared sources in /src/moonlight-ui resolve dependencies through /src/node_modules.
RUN pnpm install --frozen-lockfile \
    && ln -s /src/moonlight-admin/node_modules /src/node_modules

COPY moonlight/moonlight-admin/ ./
RUN pnpm run build


FROM ui-build-base AS editor-ui-build

COPY moonlight/moonlight-ui/ /src/moonlight-ui/
WORKDIR /src/moonlight-editor
COPY moonlight/moonlight-editor/package.json moonlight/moonlight-editor/pnpm-lock.yaml moonlight/moonlight-editor/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile \
    && ln -s /src/moonlight-editor/node_modules /src/node_modules

COPY moonlight/moonlight-editor/ ./
RUN pnpm run build


FROM docker.twelveiterations.com/seleneworlds/selene:main

COPY --exclude=illarion-gobaith-ui bundles/ /app/bundles/
COPY moonlight/ /app/bundles/
COPY bundles/illarion-gobaith-ui/illarion-client-core/ /app/bundles/illarion-client-core/
COPY bundles/illarion-gobaith-ui/illarion-gobaith-ui/ /app/bundles/illarion-gobaith-ui/
COPY --from=ui-build /src/illarion-gobaith-ui/illarion-gobaith-ui/client/ui/dist/ /app/bundles/illarion-gobaith-ui/client/ui/dist/
COPY --from=admin-ui-build /src/moonlight-admin/client/ui/dist/ /app/bundles/moonlight-admin/client/ui/dist/
COPY --from=editor-ui-build /src/moonlight-editor/client/ui/dist/ /app/bundles/moonlight-editor/client/ui/dist/
COPY server.properties script.properties /app/
