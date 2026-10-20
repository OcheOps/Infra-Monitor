"""Serves a GoReleaser dist/ directory with GitHub Releases' URL layout, so
install.sh can be tested before anything is published.

Usage: fake-release-server.py DIST_DIR VERSION PORT
  GET /latest                      -> 302 to /tag/VERSION
  GET /download/VERSION/<file>     -> DIST_DIR/<file>
"""
import functools
import http.server
import sys

dist, version, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
prefix = f"/download/{version}/"


class Handler(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/latest":
            self.send_response(302)
            self.send_header("Location", f"/tag/{version}")
            self.end_headers()
        elif self.path.startswith("/tag/"):
            self.send_response(200)
            self.end_headers()
        elif self.path.startswith(prefix):
            self.path = "/" + self.path[len(prefix):]
            super().do_GET()
        else:
            self.send_error(404)

    do_HEAD = do_GET

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", port), functools.partial(Handler, directory=dist)).serve_forever()
