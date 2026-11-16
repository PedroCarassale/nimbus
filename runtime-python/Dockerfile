# Nimbus Functions — Python 3.12 runtime
# The user zip is mounted at /var/task. The bootstrap implements the same
# one-shot and warm line protocol as runtime-node.

FROM python:3.12-alpine

WORKDIR /var/runtime

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    NIMBUS_HANDLER_PATH=/var/task/index.py \
    NIMBUS_EVENT_PATH=/var/task/event.json

COPY bootstrap.py /var/runtime/bootstrap.py

# Warm containers are started with --user node. The name has to exist in
# this image; uid 1000 matches the Node image.
RUN adduser -D -u 1000 -h /tmp node

ENTRYPOINT ["python3", "-B", "/var/runtime/bootstrap.py"]
