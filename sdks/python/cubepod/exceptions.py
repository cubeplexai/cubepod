"""CubePod Python SDK exception types."""


class CubePodFileTooLargeError(ValueError):
    """Raised when a streamed file read exceeds the configured max_bytes limit."""
