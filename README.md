# assetserver

TODO: describe

## TODO

* Reconsider "prod" vs. "dev" naming.
  - Instead of prod: static, caching, ...?
  - Instead of dev: dynamic, ...?
  - Or get rid of the distinction. Perhaps use mtimes to detect change? (Yuck.)
* Automatic gzipping?
  - Store in memory? Everything or some kind of size-limited cache?
  - Or store on disk? Then we need to provide a writeable dir. Same question
    about limiting the cache size.
  - Note that CDNs can apply compression. (Cloudflare does automatically.) So in
    that scenario this isn't all that valuable.
