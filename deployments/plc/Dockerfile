FROM docker.io/node:24-alpine@sha256:d32cdf619f63fe0471182d08996dd516c6275bb5fd31ae06e55a570bd9e1ad43

RUN apk add --no-cache dumb-init

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci --omit=dev

ENV NODE_ENV=production
ENV PORT=2582

EXPOSE 2582

USER node

ENTRYPOINT ["dumb-init", "--"]
CMD ["node", "node_modules/@did-plc/server/dist/bin.js"]
