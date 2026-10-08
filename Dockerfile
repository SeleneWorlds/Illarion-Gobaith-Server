# syntax=docker/dockerfile:1

FROM node:22-alpine AS ui-build

WORKDIR /src
COPY bundles/illarion-gobaith-ui/package*.json ./
RUN npm ci

COPY bundles/illarion-gobaith-ui/ ./
RUN npm run build


FROM node:22-alpine AS admin-ui-build

WORKDIR /src
COPY bundles/moonlight-admin/package*.json ./
RUN npm ci

COPY bundles/moonlight-admin/ ./
RUN npm run build


FROM node:22-alpine AS editor-ui-build

WORKDIR /src
COPY bundles/moonlight-editor/package*.json ./
RUN npm ci

COPY bundles/moonlight-editor/ ./
RUN npm run build


FROM docker.twelveiterations.com/seleneworlds/selene:main

COPY bundles/ /app/bundles/
COPY --from=ui-build /src/client/ui/dist/ /app/bundles/illarion-gobaith-ui/client/ui/dist/
COPY --from=admin-ui-build /src/client/ui/dist/ /app/bundles/moonlight-admin/client/ui/dist/
COPY --from=editor-ui-build /src/client/ui/dist/ /app/bundles/moonlight-editor/client/ui/dist/
COPY server.properties script.properties /app/
