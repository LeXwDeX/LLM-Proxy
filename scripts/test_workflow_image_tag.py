"""Exercise the archive workflow's actual tag-selection shell without GitHub."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class WorkflowImageTagTests(unittest.TestCase):
    def run_tag(self, value, directory):
        workflow = (Path(__file__).resolve().parents[1] / '.github/workflows/docker-build.yml').read_text()
        block = workflow.split('- name: Determine image tag', 1)[1].split('- name: Update model catalog', 1)[0]
        source = block.split('run: |', 1)[1]
        lines = source.splitlines()
        indent = min(len(line) - len(line.lstrip()) for line in lines if line.strip())
        source = '\n'.join(line[indent:] for line in lines)
        # Model GitHub's string interpolation in the legacy implementation.
        source = source.replace('${{ github.event_name }}', 'workflow_dispatch').replace('${{ github.event.inputs.image_tag }}', value)
        env = dict(os.environ, EVENT_NAME='workflow_dispatch', IMAGE_TAG_INPUT=value, GITHUB_OUTPUT=str(directory / 'output'), GITHUB_REF='refs/tags/v-fixture')
        return subprocess.run(['bash', '-eu', '-c', source], env=env, capture_output=True, text=True)

    def test_untrusted_tag_never_becomes_shell_code(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            marker = directory / 'executed'
            result = self.run_tag('$(touch ' + str(marker) + ')', directory)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(marker.exists())

    def test_valid_tag_is_preserved(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            result = self.run_tag('v1.2.3-fixture', directory)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((directory / 'output').read_text(), 'tag=v1.2.3-fixture\n')


if __name__ == '__main__':
    unittest.main()
