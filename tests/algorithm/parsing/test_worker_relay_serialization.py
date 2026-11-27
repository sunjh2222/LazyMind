import os
import subprocess
import sys


def test_module_entrypoint_worker_can_be_serialized_for_relay(tmp_path):
    # The CLI defines its worker subclass in __main__, so RelayServer
    # serializes the subclass and any globals its methods reference.
    result = subprocess.run(
        [sys.executable, '-c', """
import runpy
from lazyllm.common.utils import dump_obj, load_obj
from lazyllm.tools.rag.parsing_service import DocumentProcessorWorker

def check_relay_payload(worker):
    impl = worker._worker_impl._impl._m
    assert type(impl).__module__ == '__main__'
    assert load_obj(dump_obj(impl))._lazymind_isolated
    raise SystemExit(0)

DocumentProcessorWorker.start = check_relay_payload
runpy.run_module('lazymind.processor.service.worker', run_name='__main__', alter_sys=True)
raise AssertionError('worker entrypoint did not hand off to the relay')
"""],
        env={
            **os.environ,
            'LAZYMIND_DATABASE_URL': f'sqlite:///{tmp_path / "worker.db"}',
            'LAZYMIND_HOME': str(tmp_path / 'lazymind'),
            'LAZYLLM_HOME': str(tmp_path / 'lazyllm'),
        },
        capture_output=True,
        text=True,
        timeout=45,
    )
    assert result.returncode == 0, result.stdout + result.stderr
