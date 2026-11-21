"""Flag pathlib publication methods that Ruff's banned API rule cannot see."""

import ast
import sys
from pathlib import Path
import tokenize
import tomllib


def main(paths):
    with Path(__file__).with_name('ruff-safefs.toml').open('rb') as config:
        pattern, = tomllib.load(config)['lint']['per-file-ignores']
    # Ruff's negated brace list contains the exact files that opt in.
    scoped = set(pattern.removeprefix('!{').removesuffix('}').split(','))
    failed = False
    for path in sorted(scoped.intersection(paths)):
        with tokenize.open(path) as source:
            code = source.read()
            source.seek(0)
            allowed = {token.start[0] for token in tokenize.generate_tokens(source.readline)
                       if token.type == tokenize.COMMENT
                       and token.string.startswith('# safefs: allow ')
                       and token.string.removeprefix('# safefs: allow ').strip()}
        for node in ast.walk(ast.parse(code, filename=path)):
            if (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                    and node.func.attr in ('replace', 'rename') and len(node.args) == 1
                    and not node.keywords and node.lineno not in allowed):
                print(f'{path}:{node.lineno}: Publish files with safefs.write_file or safefs.create', file=sys.stderr)
                failed = True
    return int(failed)


if __name__ == '__main__':
    raise SystemExit(main(sys.argv[1:]))
