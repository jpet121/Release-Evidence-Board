"""Qualify CSS application measurement, then observe real isolated app journeys."""
import argparse
import base64
import datetime
import json
import re
import tempfile
import urllib.request
from collections.abc import Iterable
from pathlib import Path
from typing import Any

import browser_probe as browser
from css_coverage import coverage_report, digest, source_map


class Observer:
    def __init__(self, sources: list[dict[str, Any]], expected: dict[str, str], documents: dict[str, str] | None = None) -> None:
        self.sources = sources
        self.expected = expected
        self.documents = documents or {}
        self.observations: list[dict[str, Any]] = []
        self.pages: list[dict[str, Any]] = []
        self.script = Path(__file__).with_name('css_observer.js').read_text(encoding='utf-8')

    def sample(self, driver: browser.WebDriver, event: str) -> None:
        browser.require(all(digest(s['text']) == s['sha256'] for s in self.sources), 'source hash mismatch')
        for href in driver.script('return [...document.styleSheets].map(s=>s.href).filter(Boolean)'):
            with urllib.request.urlopen(href, timeout=10) as response:
                raw = response.read()
                text = raw.decode('utf-8').replace('\r\n', '\n')
            matched = [s for s in self.sources if s['text'] == text]
            browser.require(len(matched) == 1, 'external stylesheet bytes differ from mapped source')
            browser.require(browser.sha(raw) == self.documents.get(matched[0]['path']), 'external stylesheet raw byte digest differs from source document')
            matched[0]['href'] = href
        value = driver.call('POST', '/execute/async', {'script': self.script, 'args': [self.sources]})
        browser.require('error' not in value and not value['errors'], 'CSS observation refused: '+json.dumps(value))
        driver.script('window.rebCSS.scan(arguments[0])', event)
        value = driver.script('return window.rebCSS.read()')
        browser.require(not value['errors'], 'CSS scan refused: '+str(value['errors']))
        self.observations.extend(value['observations'])
        self.pages.append({'url': driver.current_url(), 'event': event, 'presentationUnchanged': value['presentationUnchanged']})

    def print_pdf(self, driver: browser.WebDriver, path: Path) -> None:
        self.sample(driver, 'before-native-print')
        raw = base64.b64decode(driver.call('POST', '/print', {'background': True}))
        browser.require(raw.startswith(b'%PDF-') and len(raw) > 1000, 'native print did not produce a PDF')
        path.write_bytes(raw)
        self.sample(driver, 'after-native-print')

    def report(self) -> dict[str, Any]:
        return coverage_report(self.sources, self.observations, self.expected)


def qualify(driver: browser.WebDriver, folder: Path) -> list[dict[str, Any]]:
    controls: list[dict[str, Any]] = []

    def fixture(name: str, css: str, markup: str, widths: tuple[int, ...] = (1262,), printing: bool = False) -> Observer:
        mapped = source_map(name+'.css', css)
        path = folder/(name+'.html')
        path.write_text('<!doctype html><style>'+css+'</style>'+markup, encoding='utf-8')
        driver.navigate(path.as_uri())
        observer = Observer([mapped], {name+'.css': digest(css)})
        for width in widths:
            browser.require(driver.viewport(width, 900)['width'] == width, 'native viewport unavailable')
            observer.sample(driver, name)
        if printing:
            observer.print_pdf(driver, folder/(name+'.pdf'))
        return observer

    observer = fixture('incomplete', '.parent{color:red}.missing{color:blue}\n@media print{.parent{color:black}}',
                       '<div class=parent>Parent<span>Inherited child</span></div>')
    value = observer.report()
    browser.require(value['status'] == 'FAIL' and value['line'] == {'covered': 0, 'total': 2}
                    and value['branch'] == {'covered': 1, 'total': 2}, 'incomplete fixture got false coverage')
    controls.append({'name': 'same-line-unmatched-and-print-true-missing', 'report': value})
    observer = fixture('overridden', '.parent{color:red}\n.parent{color:blue}', '<div class=parent>Override participates</div>')
    browser.require(observer.report()['line'] == {'covered': 2, 'total': 2}, 'overridden declaration lost participation')
    controls.append({'name': 'overridden-accepted-declaration-participates', 'report': observer.report()})
    observer = fixture('complete', '.parent{color:red}\n@media(max-width:820px){.parent{color:blue}}\n@media(max-width:420px){.parent{color:green}}\n@media print{.parent{color:black}}',
                       '<div class=parent>Print qualification</div>', (821, 820, 421, 420), True)
    value = observer.report()
    browser.require(value['status'] == 'PASS' and value['line'] == {'covered': 4, 'total': 4}
                    and value['branch'] == {'covered': 6, 'total': 6}, 'native complete vector lacks actual print application')
    controls.append({'name': 'native-width-boundaries-and-print', 'report': value,
                     'observations': observer.observations})
    for name, css in [('invalid-declaration', '.parent{color:INVALID_VALUE}'),
                      ('collision', '.parent{--reb-application-collision:1;color:red}')]:
        try:
            fixture(name, css, '<div class=parent>Refusal</div>')
        except AssertionError as error:
            expected = 'invalid authored CSS declaration' if name == 'invalid-declaration' else 'marker collision'
            browser.require(expected in str(error), 'qualification failed for an unrelated reason: '+str(error))
            controls.append({'name': name, 'refused': str(error)})
        else:
            raise AssertionError(name+' was not refused')
    observer = fixture('nested-inactive', '@media(max-width:420px){@media print{.parent{color:red}}}',
                       '<div class=parent>Inactive ancestor</div>', printing=True)
    browser.require(all(all(i != 1 for i, _ in o['outcomes']) for o in observer.observations), 'inactive ancestor earned inner outcome')
    controls.append({'name': 'inactive-ancestor-no-inner-outcome', 'report': observer.report()})
    observer = fixture('inheritance', '.parent{color:red}', '<div class=parent>Parent<span>Child</span></div>')
    marker = '--reb-application-'+observer.sources[0]['sha256'][:16]+'-0'
    browser.require(driver.script('return getComputedStyle(document.querySelector("span")).getPropertyValue(arguments[0]).trim()', marker) == '0',
                    'registered marker inherited a false child hit')
    controls.append({'name': 'noninherited-child-marker-zero', 'observed': '0'})
    observer.sources[0]['sha256'] = '0'*64
    try:
        observer.sample(driver, 'tampered source hash')
    except AssertionError as error:
        browser.require('source hash mismatch' in str(error), 'wrong source-hash refusal')
        controls.append({'name': 'source-hash-mismatch', 'refused': str(error)})
    else:
        raise AssertionError('wrong source hash accepted')
    observer = Observer([], {})
    try:
        observer.sample(driver, 'omitted source')
    except AssertionError as error:
        browser.require('unmapped active CSS stylesheet' in str(error), 'wrong omitted-source refusal')
        controls.append({'name': 'omitted-active-source', 'refused': str(error)})
    else:
        raise AssertionError('omitted active source accepted')
    observer = fixture('dynamic', '.parent{color:red}', '<div class=parent>Dynamic source</div>')
    driver.script('document.styleSheets[0].disabled=true')
    try:
        observer.sample(driver, 'disabled source')
    except AssertionError as error:
        browser.require('active stylesheet changed after binding' in str(error), 'wrong disabled-source refusal')
        controls.append({'name': 'disabled-source-after-binding', 'refused': str(error)})
    else:
        raise AssertionError('disabled stylesheet accepted')
    try:
        source_map('unsupported.css', '@supports(display:grid){.parent{display:grid}}')
    except ValueError as error:
        controls.append({'name': 'unsupported-syntax-blocked', 'refused': str(error)})
    else:
        raise AssertionError('unsupported syntax accepted')
    return controls


def production(driver: browser.WebDriver, args: argparse.Namespace, folder: Path, evidence: Path) -> dict[str, Any]:
    root = args.root.resolve()
    sources: list[dict[str, Any]] = []
    for path in sorted((root/'internal').rglob('*')):
        if path.suffix == '.css':
            sources.append(source_map(path.relative_to(root).as_posix(), path.read_text(encoding='utf-8')))
        elif path.suffix == '.html':
            text = path.read_text(encoding='utf-8')
            for index, match in enumerate(re.finditer(r'<style\b[^>]*>([\s\S]*?)</style\s*>', text, re.IGNORECASE)):
                sources.append(source_map(path.relative_to(root).as_posix()+'#style-'+str(index), match[1], 1+text.count('\n', 0, match.start(1))))
    scope = json.loads(args.inventory.read_text(encoding='utf-8-sig'))
    browser.require(scope['method'] == 'reb-css-application/1', 'unqualified CSS scope method')
    for document in scope['documents']:
        browser.require(browser.sha((root/document['path']).read_bytes()) == document['sha256'], 'CSS source document changed')
    observer = Observer(sources, scope['segments'], {d['path']: d['sha256'] for d in scope['documents']})
    observer.report()  # Refuse omitted or extra source before any browser hit.
    original = browser.record_step
    record: dict[str, Any] = {'steps': [], 'inputHashes': {}}

    def record_step(drv: browser.WebDriver, record: browser.JsonObject, name: str, expected: Iterable[str] = ()) -> browser.JsonObject:
        result = original(drv, record, name, expected)
        observer.sample(drv, name)
        if '200%' in name or '320 CSS pixels' in name:
            return result
        # Exercise genuine interaction states on the existing document. No
        # fabricated DOM or rewritten selector is allowed for production hits.
        for selector in ['nav a', 'button', '.secondary', 'a']:
            node = drv.script('''for(const e of document.querySelectorAll(arguments[0])) {
                e.scrollIntoView({block:'center'}); const r=e.getBoundingClientRect();
                if(r.width>0 && r.height>0 && r.top>=0 && r.left>=0 && r.bottom<=innerHeight && r.right<=innerWidth) return e;
                } return null;''', selector)
            if node:
                drv.call('POST', '/actions', {'actions': [{'type': 'pointer', 'id': 'css-mouse', 'parameters': {'pointerType': 'mouse'},
                    'actions': [{'type': 'pointerMove', 'duration': 0, 'origin': node, 'x': 0, 'y': 0}]}]})
                observer.sample(drv, name+' hover '+selector)
        drv.keys('\ue004')
        observer.sample(drv, name+' keyboard')
        if drv.nodes('.actions label') or drv.nodes('aside'):
            for width in [820, 420]:
                browser.require(drv.viewport(width, 900)['width'] == width, 'native production viewport unavailable')
                observer.sample(drv, name+' width '+str(width))
            drv.viewport(1262, 900)
        if drv.script('return document.querySelector("style")?.textContent.includes("@media print") || false'):
            observer.print_pdf(drv, evidence/('report-'+str(len(record['steps']))+'.pdf'))
        return result

    original_save = browser.save_and_read_back_draft

    def save_draft(drv: browser.WebDriver, edited: str, selector: str = 'textarea[name=expected_0]', save_button: str | None = None) -> dict[str, int]:
        observer.sample(drv, 'editable behavior and test fields before save')
        return original_save(drv, edited, selector, save_button)

    browser.record_step = record_step
    browser.save_and_read_back_draft = save_draft
    try:
        with browser.application(args, folder, evidence) as (base, binary_hash, alternate, suspend):
            browser.core_journeys(driver, base, args, alternate, suspend, folder, evidence, record)
    finally:
        browser.record_step = original
        browser.save_and_read_back_draft = original_save
    # A second owned database exercises actual expired PASS evidence, without
    # waiting on or changing either account's import window in the main journey.
    stale_folder = folder/'stale-fixture'
    stale_folder.mkdir()
    stale_evidence = evidence/'stale'
    stale_evidence.mkdir()
    with browser.application(args, stale_folder, stale_evidence) as (base, _, _, _):
        browser.sign_in(driver, base, args.username, args.password)
        driver.click_label('Baselines')
        driver.type('input[name=baseline_id]', 'B1')
        driver.click_label('Create draft')
        driver.click_label('Add behavior')
        for name, value in {'behavior_id': 'BEH1', 'policy_id': 'POL1', 'minimum': '10000', 'max_age': '24',
                            'test_0': 'T1', 'attempts_0': '1'}.items():
            driver.type('input[name='+name+']', value)
        driver.type('textarea[name=text]', 'Reject unauthorized reads')
        driver.type('textarea[name=expected_0]', 'Access denied')
        driver.set_checkbox('input[name=required]', True)
        driver.set_checkbox('input[name=must_pass_0]', True)
        browser.save_and_read_back_draft(driver, 'Reject unauthorized reads', 'textarea[name=text]')
        driver.click_label('Approve saved draft')
        driver.click_label('Candidates')
        driver.click_label('Create candidate')
        browser.fill_candidate(driver, 'R1', 'E1')
        expired = browser.source_fixture(stale_folder, record, 'css-expired', 'PASS', datetime.datetime.now(datetime.UTC)-datetime.timedelta(days=2))
        browser.preview(driver, expired)
        browser.confirm(driver)
        browser.require(driver.nodes('.status.STALE'), 'expired complete PASS did not render STALE')
        observer.sample(driver, 'expired complete run is STALE, candidate BLOCKED')
    result = observer.report()
    result.update(observations=observer.observations, pages=observer.pages, steps=record['steps'], application_sha256=binary_hash,
                  source_documents=scope['documents'], source_maps=sources)
    return result


def main() -> int:
    parser = argparse.ArgumentParser()
    for name in ['root', 'application', 'browser', 'driver', 'output', 'inventory']:
        parser.add_argument('--'+name, required=True, type=Path)
    parser.add_argument('--kind', required=True)
    parser.add_argument('--qualification-only', action='store_true')
    args = parser.parse_args()
    args.base_url, args.project = None, 'css-isolated'
    args.username, args.password = 'reviewer', 'css-synthetic-reviewer-password'
    args.ai_off_sdk, args.rollback_proof = None, None
    args.output.mkdir(parents=True, exist_ok=False)
    result: dict[str, Any] = {'status': 'BLOCKED', 'method': 'reb-css-application/1'}
    try:
        with tempfile.TemporaryDirectory(prefix='reb-css-') as temporary, (args.output/'driver.log').open('wb') as log:
            folder = Path(temporary)
            with browser.WebDriver(args.driver, args.browser, args.kind, log) as driver:
                result['capabilities'] = driver.capabilities
                result['qualification'] = qualify(driver, args.output)
                if not args.qualification_only:
                    result['production'] = production(driver, args, folder, args.output)
                result['status'] = result.get('production', {'status': 'PASS'})['status']
    except browser.ProbeFailure as error:
        result['status'] = 'BLOCKED'
        result['error'] = str(error)
    report_path = args.output/'results.json'
    report_path.write_text(json.dumps(result, indent=2)+'\n', encoding='utf-8')
    print(json.dumps({'status': result['status'], 'error': result.get('error'),
                      'native_report_sha256': browser.sha(report_path.read_bytes()),
                      'pdf_artifacts': [{'path': path.relative_to(args.output).as_posix(), 'sha256': browser.sha(path.read_bytes())}
                                        for path in sorted(args.output.rglob('*.pdf'))],
                      'production': {k: result.get('production', {}).get(k) for k in ['line','branch','files']}}))
    return 0 if result['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
