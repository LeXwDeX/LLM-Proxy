import unittest
from unittest.mock import patch
from scripts import verify_azure_v1_models as verifier

class AzureProbeContractTest(unittest.TestCase):
    def test_empty_config_is_not_a_successful_verification(self):
        with patch('sys.argv', ['verify']), patch.object(verifier, 'load_config', return_value={}), patch.object(verifier, 'probe') as probe:
            self.assertEqual(verifier.main(), 2)
            probe.assert_not_called()

class AzureRedirectContractTest(unittest.TestCase):
    def test_probe_preserves_redirect_status_without_forwarding_credentials(self):
        import http.server
        import threading
        from contextlib import ExitStack
        forwarded = []

        class Destination(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                forwarded.append(self.headers.get('api-key'))
                self.send_response(200)
                self.end_headers()
            def log_message(self, *_):
                pass

        class Redirect(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(302)
                self.send_header('Location', destination_url)
                self.end_headers()
            def log_message(self, *_):
                pass

        with ExitStack() as stack:
            destination = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Destination)
            destination_url = 'http://127.0.0.1:' + str(destination.server_port) + '/model'
            redirect = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Redirect)
            for server in (destination, redirect):
                thread = threading.Thread(target=server.serve_forever, daemon=True)
                thread.start()
                stack.callback(server.server_close)
                stack.callback(server.shutdown)
            result = verifier.probe('fixture', 'http://127.0.0.1:' + str(redirect.server_port), 'synthetic-only', 'qwen-max', 3, 5)
            self.assertEqual(result['status'], '302')
            self.assertFalse(result['ok'])
            self.assertEqual(forwarded, [])
