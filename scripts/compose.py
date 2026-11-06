"""Inline CSS, Go's wasm_exec.js and the app script into one page.

Writes dist/index.html (a full document, for Vercel) and out/artifact.html
(the same page without the html/head/body skeleton).
"""
import pathlib, sys

root = pathlib.Path(sys.argv[1])
src = root / "web" / "src"
body = (src / "body.html").read_text(encoding="utf-8")
page = (body.replace("/*__STYLE__*/", (src / "style.css").read_text(encoding="utf-8"))
            .replace("/*__WASM_EXEC__*/", (src / "wasm_exec.js").read_text(encoding="utf-8"))
            .replace("/*__APP__*/", (src / "app.js").read_text(encoding="utf-8")))
dist = root / "dist"
dist.mkdir(exist_ok=True)
out = root / "out"
out.mkdir(exist_ok=True)
(out / "artifact.html").write_text(page, encoding="utf-8")
full = ('<!doctype html>\n<html lang="en">\n<head>\n<meta charset="utf-8">\n'
        '<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">\n'
        '<style>:root{padding-top:env(safe-area-inset-top,0px);padding-bottom:env(safe-area-inset-bottom,0px)}'
        'body{margin:0}[hidden]{display:none!important}</style>\n</head>\n<body>\n' + page + '\n</body>\n</html>\n')
(dist / "index.html").write_text(full, encoding="utf-8")
print("wrote", dist / "index.html", len(full), "bytes")
