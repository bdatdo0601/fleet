import sys

if sys.version_info < (3, 9):  # noqa: UP036 - runs before the 3.9+ code is imported
    sys.exit("fleet needs Python 3.9+ (brew install python)")

from .cli import main

sys.exit(main())
