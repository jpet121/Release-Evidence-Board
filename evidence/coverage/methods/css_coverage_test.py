"""Public CSS measurement contract: source obligations cannot vanish."""
import unittest

from css_coverage import coverage_report, source_map


class CSSContract(unittest.TestCase):
    def test_same_line_requires_every_rule_and_each_condition_outcome(self) -> None:
        mapped = source_map('fixture.css', 'a{color:red} b{color:blue}\n@media print{a{color:black}}')
        report = coverage_report([mapped], [{'source': mapped['sha256'], 'rules': [0], 'outcomes': [[2, False]]}], {mapped['path']: mapped['sha256']})
        self.assertEqual(report['line'], {'covered': 0, 'total': 2})
        self.assertEqual(report['branch'], {'covered': 1, 'total': 2})
        self.assertEqual(report['status'], 'FAIL')

    def test_complete_literal_vector(self) -> None:
        mapped = source_map('fixture.css', 'a{color:red}\n@media print{a{color:black}}', 9)
        report = coverage_report([mapped], [{'source': mapped['sha256'], 'rules': [0, 2], 'outcomes': [[1, False], [1, True]]}], {mapped['path']: mapped['sha256']})
        self.assertEqual(report['line'], {'covered': 2, 'total': 2})
        self.assertEqual(report['branch'], {'covered': 2, 'total': 2})
        self.assertEqual(report['files'][0]['lines'], [9, 10])
        self.assertEqual(report['status'], 'PASS')

    def test_missing_source_and_unbound_observation_are_not_passes(self) -> None:
        mapped = source_map('fixture.css', 'a{color:red}')
        self.assertEqual(coverage_report([mapped], [], {mapped['path']: mapped['sha256']})['status'], 'BLOCKED')
        with self.assertRaises(ValueError):
            coverage_report([], [], {})
        with self.assertRaises(ValueError):
            coverage_report([mapped], [{'source': '0'*64, 'rules': [0], 'outcomes': []}], {mapped['path']: mapped['sha256']})

    def test_unknown_syntax_and_invalid_native_id_fail_closed(self) -> None:
        for css in ['@supports(display:grid){a{display:grid}}', 'a{color:red', 'a{}', 'a{color:red} garbage',
                    '/* mapping unknown */a{color:red}', 'a{color:rgb(1,2,3}']:
            with self.subTest(css=css), self.assertRaises(ValueError):
                source_map('fixture.css', css)
        mapped = source_map('fixture.css', 'a{color:red}')
        with self.assertRaises(ValueError):
            coverage_report([mapped], [{'source': mapped['sha256'], 'rules': [1], 'outcomes': []}], {mapped['path']: mapped['sha256']})

    def test_strings_and_functions_do_not_split_source_positions(self) -> None:
        mapped = source_map('fixture.css', 'a{content:"a;}b";color:rgb(1,2,3)}\nb{color:red}', 8)
        self.assertEqual(mapped['rules'][0]['declarations'], [['content', '"a;}b"'], ['color', 'rgb(1,2,3)']])
        self.assertEqual(mapped['rules'][1]['lines'], [9])

    def test_blank_and_closing_braces_are_not_executable_lines(self) -> None:
        mapped = source_map('fixture.css', 'a {\n\n color: red;\n}')
        self.assertEqual(mapped['rules'][0]['lines'], [1, 3])
        self.assertEqual(mapped['rules'][0]['declaration_lines'], [[3]])
        report = coverage_report([mapped], [{'source': mapped['sha256'], 'rules': [0], 'outcomes': []}], {mapped['path']: mapped['sha256']})
        self.assertEqual(report['line'], {'covered': 2, 'total': 2})
        self.assertEqual(report['status'], 'BLOCKED')

    def test_omitted_expected_source_cannot_disappear(self) -> None:
        mapped = source_map('one.css', 'a{color:red}')
        with self.assertRaises(ValueError):
            coverage_report([mapped], [], {'one.css': mapped['sha256'], 'omitted.css': '1'*64})

    def test_flat_file_zero_is_not_hidden_by_another_file(self) -> None:
        flat = source_map('flat.css', 'a{color:red}')
        media = source_map('media.css', '@media print{a{color:blue}}')
        report = coverage_report([flat, media], [
            {'source': flat['sha256'], 'rules': [0], 'outcomes': []},
            {'source': media['sha256'], 'rules': [1], 'outcomes': [[0, False], [0, True]]},
        ], {flat['path']: flat['sha256'], media['path']: media['sha256']})
        self.assertEqual(report['status'], 'BLOCKED')


if __name__ == '__main__':
    unittest.main()
