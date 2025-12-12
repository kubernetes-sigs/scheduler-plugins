import contextlib
import io
import logging
from dataclasses import dataclass, field
from typing import Callable, Iterable, Iterator, List, Tuple


def make_logger_stream(name_prefix: str, level: int = logging.DEBUG) -> Tuple[logging.Logger, io.StringIO]:
    """Create an isolated logger + backing StringIO stream for assertions."""
    stream = io.StringIO()
    logger = logging.getLogger(f"test-{name_prefix}-{id(stream)}")
    logger.handlers.clear()
    logger.setLevel(level)
    logger.propagate = False
    handler = logging.StreamHandler(stream)
    handler.setFormatter(logging.Formatter("%(levelname)s %(message)s"))
    logger.addHandler(handler)
    return logger, stream


@contextlib.contextmanager
def null_lock() -> Iterator[None]:
    """A do-nothing context manager used to stub out file locks."""
    yield


@dataclass
class TimeController:
    """Deterministic time controller for tests that patch time.time/time.sleep."""

    now: float = 0.0
    sleeps: List[float] = field(default_factory=list)

    def time(self) -> float:
        return float(self.now)

    def sleep(self, dt: float) -> None:
        dt_f = float(dt)
        self.sleeps.append(dt_f)
        self.now += dt_f


def time_sequence(values: Iterable[float]) -> Callable[[], float]:
    """Return a time.time() replacement that yields a fixed sequence."""
    it = iter(values)

    def _time() -> float:
        return float(next(it))

    return _time