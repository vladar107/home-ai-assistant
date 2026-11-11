FROM alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f AS relay
RUN apk add --no-cache tinyproxy socat
USER 65534:65534

# An egress proxy carries its server's host list in the image: the NAS admits only the deploying
# account to the deployment tree, so a container running as another user cannot read a bind mount,
# and files copied from there need their mode set here.
FROM relay AS egress
ARG SERVER
COPY --chmod=0444 proxy/egress.conf /etc/tinyproxy/tinyproxy.conf
COPY --chmod=0444 ${SERVER}/egress.filter /etc/tinyproxy/filter
CMD ["tinyproxy", "-d", "-c", "/etc/tinyproxy/tinyproxy.conf"]
