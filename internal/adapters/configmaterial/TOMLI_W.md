# Embedded TOML writer

`tomli_w.py` is the unmodified `_writer.py` from
[Tomli-W 1.2.0](https://github.com/hukkin/tomli-w/tree/1.2.0), distributed under
the MIT license in `TOMLI_W_LICENSE`. It is embedded with the ownership helper so
updating an existing yard does not require installing a Python package before
the release's read-only configuration observation can run.

Parsing uses Python's standard `tomllib`; the writer runs in a separate namespace
and is only loaded for TOML requests. When updating the vendored writer, retain
the upstream source and license together and run the configuration materialization
tests, including TOML runtime additions, typed values and invalid input handling.
