"""Source-mapped CSS application obligations, AC-024-06 approved alternative.

This deliberately small grammar accepts style rules and media groups. Unknown
syntax blocks measurement instead of silently dropping authored contributions.
Native parsing and application are separately checked by css_observer.js.
"""
import hashlib
import json
import re
import sys
from pathlib import Path
from typing import Any


def digest(text: str) -> str:
    return hashlib.sha256(text.encode('utf-8')).hexdigest()


def source_map(path: str, text: str, first_line: int = 1) -> dict[str, Any]:
    rules: list[dict[str, Any]] = []

    def delimiter(start: int, end: int, wanted: str) -> int:
        quote, depth = '', 0
        i = start
        while i < end:
            c = text[i]
            if quote:
                if c == '\\':
                    i += 2
                    continue
                if c == quote:
                    quote = ''
            elif c in '\"\'':
                quote = c
            elif text[i:i+2] == '/*':
                raise ValueError('CSS comments require an explicit source mapping extension')
            elif c in '([':
                depth += 1
            elif c in ')]':
                depth -= 1
                if depth < 0:
                    raise ValueError('unbalanced CSS expression')
            elif depth == 0 and c in wanted:
                return i
            i += 1
        if quote or depth:
            raise ValueError('unterminated CSS expression')
        return end

    def parse(start: int, end: int, parents: list[int], indexes: list[int]) -> None:
        cursor, index = start, 0
        while cursor < end:
            while cursor < end and text[cursor].isspace():
                cursor += 1
            if cursor == end:
                break
            opening = delimiter(cursor, end, '{;}')
            if opening == end or text[opening] != '{':
                raise ValueError('unsupported or incomplete CSS rule')
            head = text[cursor:opening].strip()
            close, nesting = opening+1, 1
            while nesting:
                boundary = delimiter(close, end, '{}')
                if boundary == end:
                    raise ValueError('unclosed CSS rule')
                nesting += 1 if text[boundary] == '{' else -1
                close = boundary+1
            identifier = len(rules)
            row: dict[str, Any] = {'id': identifier, 'index': indexes+[index], 'parents': parents}
            def positions(a: int, b: int) -> list[int]:
                start_line = first_line+text.count('\n', 0, a)
                return [start_line+n for n, line in enumerate(text[a:b].split('\n')) if line.strip(' \t\r\n{}')]
            if head.startswith('@media') and re.fullmatch(r'@media\s*(?:print|\(max-width:\s*\d+px\))', head):
                row.update(kind='media', condition=head[6:].strip(), lines=positions(cursor, opening))
                rules.append(row)
                parse(opening+1, close-1, parents+[identifier], indexes+[index])
            elif head.startswith('@'):
                raise ValueError('unsupported CSS conditional or at-rule: '+head)
            else:
                declarations, declaration_lines, position = [], [], opening+1
                while position < close-1:
                    boundary = delimiter(position, close-1, ';{}')
                    raw = text[position:boundary].strip()
                    if raw:
                        if ':' not in raw or (boundary < close-1 and text[boundary] != ';'):
                            raise ValueError('invalid or nested declaration')
                        name, value = raw.split(':', 1)
                        name, value = name.strip(), value.strip()
                        if not re.fullmatch(r'(?:--)?[a-zA-Z][\w-]*', name) or not value:
                            raise ValueError('invalid declaration')
                        declarations.append([name, value])
                        declaration_lines.append(positions(position, boundary))
                    position = boundary+1
                if not declarations:
                    raise ValueError('empty CSS rule has no application metric')
                row.update(kind='style', selector=head, declarations=declarations, declaration_lines=declaration_lines,
                           lines=sorted(set(positions(cursor, opening)+[line for lines in declaration_lines for line in lines])))
                rules.append(row)
            index += 1
            cursor = close
    parse(0, len(text), [], [])
    if not rules:
        raise ValueError('CSS source has no measurable rules')
    return {'path': path, 'sha256': digest(text), 'text': text, 'rules': rules}


def coverage_report(sources: list[dict[str, Any]], observations: list[dict[str, Any]],
                    expected: dict[str, str]) -> dict[str, Any]:
    if {s['path']: s['sha256'] for s in sources} != expected:
        raise ValueError('CSS source inventory differs from the independently supplied contract')
    if not sources or len({s['path'] for s in sources}) != len(sources):
        raise ValueError('missing or duplicate CSS source')
    known = {s['sha256'] for s in sources}
    if len(known) != len(sources):
        raise ValueError('ambiguous CSS source identity')
    if any(o['source'] not in known for o in observations):
        raise ValueError('unbound CSS observation')
    files: list[dict[str, Any]] = []
    for source in sources:
        hits: set[int] = set()
        outcomes: set[tuple[int, bool]] = set()
        styles = {r['id'] for r in source['rules'] if r['kind'] == 'style'}
        media = {r['id'] for r in source['rules'] if r['kind'] == 'media'}
        for observation in observations:
            if observation['source'] != source['sha256']:
                continue
            if not set(observation['rules']) <= styles:
                raise ValueError('unknown CSS rule hit')
            for identifier, value in observation['outcomes']:
                if identifier not in media or type(value) is not bool:
                    raise ValueError('unknown CSS conditional outcome')
                outcomes.add((identifier, value))
            hits.update(observation['rules'])
        lines: dict[int, bool] = {}
        for rule in source['rules']:
            hit = rule['id'] in hits if rule['kind'] == 'style' else any(i == rule['id'] for i, _ in outcomes)
            for line in rule['lines']:
                lines[line] = lines.get(line, True) and hit
        misses = [[i, v] for i in sorted(media) for v in [False, True] if (i, v) not in outcomes]
        files.append({'path': source['path'], 'sha256': source['sha256'], 'lines': sorted(lines),
                      'line': {'covered': sum(lines.values()), 'total': len(lines)},
                      'branch': {'covered': len(outcomes), 'total': 2*len(media)},
                      'missed_lines': [n for n, hit in lines.items() if not hit],
                      'missed_rules': sorted(styles-hits), 'missed_outcomes': misses})
    line = {k: sum(f['line'][k] for f in files) for k in ['covered', 'total']}
    branch = {k: sum(f['branch'][k] for f in files) for k in ['covered', 'total']}
    status = 'PASS' if line['covered'] == line['total'] and branch['covered'] == branch['total'] else 'FAIL'
    if not line['total'] or not branch['total'] or any(not f['branch']['total'] for f in files):
        status = 'BLOCKED'
    return {'method': 'reb-css-application/1', 'status': status,
            'line': line, 'branch': branch, 'exclusions': [], 'files': files}


def mapped_documents(root: Path, scope: dict[str, Any]) -> list[dict[str, Any]]:
    mapped = []
    for document in scope['documents']:
        raw = (root/document['path']).read_bytes()
        if hashlib.sha256(raw).hexdigest() != document['sha256']:
            raise ValueError('CSS source document changed')
        text = raw.decode('utf-8').replace('\r\n', '\n')
        if document['path'].endswith('.css'):
            mapped.append(source_map(document['path'], text))
        else:
            for index, match in enumerate(re.finditer(r'<style\b[^>]*>([\s\S]*?)</style\s*>', text, re.IGNORECASE)):
                mapped.append(source_map(document['path']+'#style-'+str(index), match[1], 1+text.count('\n', 0, match.start(1))))
    if {s['path']: s['sha256'] for s in mapped} != scope['segments']:
        raise ValueError('CSS source segments differ from inventory')
    return mapped


if __name__ == '__main__':
    if len(sys.argv) != 3:
        raise SystemExit('Expected source root and independently captured CSS scope JSON')
    print(json.dumps(mapped_documents(Path(sys.argv[1]), json.loads(sys.argv[2]))))
