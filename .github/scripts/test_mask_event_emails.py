import contextlib
import io
import unittest

from mask_event_emails import add_mask, redact_emails


class EventEmailTests(unittest.TestCase):
    def test_nested_event_emails_are_masked_and_removed(self):
        payload = {
            "pusher": {"email": "fixture-user@example.invalid", "name": "fixture-user"},
            "repository": {"owner": {"email": "fixture-owner@example.invalid"}},
            "commits": [{"author": {"email": "fixture-commit@example.invalid"}}],
            "unchanged": {"email": None, "id": 123},
        }
        masks = []
        redact_emails(payload, masks.append)
        self.assertEqual(len(masks), 3)
        self.assertEqual(payload["pusher"]["email"], "[redacted]")
        self.assertEqual(payload["repository"]["owner"]["email"], "[redacted]")
        self.assertEqual(payload["commits"][0]["author"]["email"], "[redacted]")
        self.assertEqual(payload["pusher"]["name"], "fixture-user")
        self.assertEqual(payload["unchanged"], {"email": None, "id": 123})

    def test_mask_command_escapes_workflow_control_characters(self):
        stream = io.StringIO()
        with contextlib.redirect_stdout(stream):
            add_mask("fixture%\r\n@example.invalid")
        self.assertEqual(stream.getvalue(), "::add-mask::fixture%25%0D%0A@example.invalid\n")


if __name__ == "__main__":
    unittest.main()
