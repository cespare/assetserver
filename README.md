# assetserver

[![Go Reference](https://pkg.go.dev/badge/github.com/cespare/assetserver.svg)](https://pkg.go.dev/github.com/cespare/assetserver)

**NOTE: this package is still under active development and has not yet reached
version 1.0.**

The assetserver package provides a file server designed for static assets.

## Usage

The simplest way to use it is as a replacement for `http.FileServer`:

    assetSrv := assetserver.New(assetFS)

where `assetFS` is an `embed.FS` or comes from `os.DirFS`. Then the main handler
or muxer routes all requests with a prefix (say, `/static/`) to `assetSrv`.

Asset files that are served this way have two caching-related headers in the
response:

* `ETag` contains a hash of the file contents
* `Cache-Control` is set to `public, max-age=60` (1 minute)

This means that clients might cache the files for up to a minute before
revalidating. Revalidation with `If-None-Match` is based on the file hash and
the server will always be able to respond with `304 Not Modified` unless the
actual file content has changed.

## Better caching with tags

By writing a bit more code, assetserver can be used to implement a near-optimal
caching strategy. Asset files may be referred to by *tagged* names: instead of,
for example, `style.css`, the client might request `style.EI7Zfw9kFp.css`. The
tag (`EI7Zfw9kFp`) is the same file hash in the `ETag` header. When the file
changes, the tag changes. When a tagged file is served, the `Cache-Control`
header is

    public, max-age=31536000, immutable

This means that files will generally be cached for as long as possible because
the content corresponding to a tagged name never changes.

Tagged asset names are constructed with `(*assetserver.Server).Tag`. This method
inserts a tag into a filename before the first dot.

Continuing our example from above, one might expose the `Tag` method as a
template function:

``` go
funcs := template.FuncMap{
	"tag": assetSrv.Tag,
}
tmpl, err := template.New("").Funcs(funcs).Parse(tmplText)
...
```

Then the HTML template might contain:

```
<link rel="stylesheet" href="{{tag "/static/css/style.css"}}">
<script src="{{tag "/static/js/main.js"}}"></script>
```

## Comparison with `http.FileServer`

The caching strategies described above are a significant advantage that
assetserver has over the standard library's file server when it comes to static
assets. Using `http.FileServer` for these types of files can result in both
under- and over-caching:

* When files are served from disk, the only caching hint the client gets is in
  the form of the `Last-Modified` header. The details depend on the particular
  heuristics of each client, but typically in this case clients cache the file
  for some fraction of the time since last modification. This means that if the
  file mtimes are changed on deploy, many clients may immediately re-request the
  full contents. It also means that if it has been a long time since the mtimes
  changed, the clients might cache the assets for a significant length of time
  and not notice that the server has updated the assets in the meantime.
* When files are served from an `embed.FS`, there is no mtime, no
  `Last-Modified` header, and clients generally won't cache the files at all.

Compared with `http.FileServer`, assetserver also leaks less internal server
information and this also makes it better-suited for serving static assets:

* It does not serve directory listings (it returns `404 Not Found` for directories)
* It does not serve `index.html` redirects
* Internal errors (such as permission errors) are not exposed as HTTP errors;
  any internal error other than "not found" becomes a 500.

## Caveats

The assetserver package is designed specifically to serve static web assets that
rarely or never change. Internally, it caches a bit of information about each
file that it has ever served. This means that there are a few usages to be
avoided:

* The server is not appropriate for serving a very large number of different
  files (say, millions)
* The internal cache never shrinks, so if new files are being created over time,
  memory usage will grow without bound even if old files are deleted.
* The internal cache uses {mtime, size} as a proxy to determine whether a file
  has changed (and therefore whether we need to recompute the hash). If a file
  changes without altering the mtime or size, or if a file is altered
  non-atomically, incorrect caching information will be served.
