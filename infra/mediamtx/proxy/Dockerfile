FROM bluenviron/mediamtx:latest-ffmpeg

RUN apk add --no-cache ffmpeg

COPY republish-srt.sh /usr/local/bin/republish-srt
RUN chmod +x /usr/local/bin/republish-srt
