"""Unit checks complement real guest/SDK execution, never replace it."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import MagicMock, patch
from types import SimpleNamespace
import os
import stat
import dmsetup_node

from cleanup_node import owned_files, validate_pool, validate_thin
from verify_guest_log import guest_ready
from verify_cep0031_gate import nfs_helper_findings
from dmsetup_node import validate as validate_publication
from collect_block_state import owned_thin as validate_diagnostic

class RuntimeProofTest(unittest.TestCase):
    def test_guest_proof_requires_both_exact_kernel_and_output(self):
        for text in ('', '6.18.62\n', 'KATA_GUEST_READY\n', '6.12.113\nKATA_GUEST_READY\n'):
            self.assertFalse(guest_ready(text))
        self.assertTrue(guest_ready('6.18.62\r\nKATA_GUEST_READY\r\n'))
        custom='6.18.62-cubepod-nfs1'
        self.assertTrue(guest_ready(custom+'\nKATA_GUEST_READY\n',custom))
        self.assertFalse(guest_ready('6.18.62\nKATA_GUEST_READY\n',custom))
        self.assertFalse(guest_ready(custom+'\nKATA_GUEST_READY\n'))

    def verify(self, runtime='kata-clh-e2e', count=12, expected='kata-clh-e2e', reports=None):
        events = [{'type':'ADDED', 'object':{'kind':'Pod','metadata':{'name':f'sandbox-{i}'},'spec':{'runtimeClassName':runtime}}} for i in range(count)]
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'events.json'
            path.write_text('\n'.join(json.dumps(e) for e in events))
            args=[sys.executable,str(Path(__file__).with_name('verify_pods.py')),str(path),expected]
            if reports:
                written=[]
                for index, body in enumerate(reports):
                    report=Path(directory)/f'report-{index}.xml'
                    report.write_text('<testsuite>'+body+'</testsuite>')
                    written.append(str(report))
                args.extend(written)
            return subprocess.run(args,capture_output=True).returncode
    def test_all_sandboxes_use_requested_runtime(self):
        self.assertEqual(self.verify(),0)
    def test_runc_fallback_fails(self):
        self.assertNotEqual(self.verify(runtime='none'),0)
    def test_partial_or_empty_execution_fails(self):
        for count in (0,1,11):self.assertNotEqual(self.verify(count=count),0)
    def test_skipped_volume_tests_lower_expected_pod_count(self):
        volume = "<testcase name='test_volume_mount_persistence'><skipped message='#120'/></testcase>"
        volume_ts = "<testcase name='volume mounts survive destruction and recreation'><skipped message='#120'/></testcase>"
        others = "<testcase name='a'/><testcase name='b'/><testcase name='c'/><testcase name='d'/>"
        reports = [others + volume, others + volume_ts]
        self.assertEqual(self.verify(count=8, reports=reports), 0)
        self.assertNotEqual(self.verify(count=7, reports=reports), 0)



class Cep0031HelperProbeTest(unittest.TestCase):
    """CEP-0031 gate: dpkg status must be parsed, not treated as presence."""
    def test_absent_states_pass(self):
        for probe in ('', 'nfs-common unknown ok not-installed', 'nfs-common deinstall ok config-files',
                      'dpkg-query: no packages found matching nfs-common', '\n'):
            self.assertEqual(nfs_helper_findings(probe), [], probe)
    def test_installed_package_fails(self):
        for probe in ('nfs-common install ok installed', 'nfs-common install ok unpacked',
                      'nfs-common install ok half-configured'):
            self.assertTrue(nfs_helper_findings(probe), probe)
    def test_mount_nfs_binary_fails(self):
        for probe in ('/usr/sbin/mount.nfs', '/sbin/mount.nfs4\nnfs-common unknown ok not-installed'):
            self.assertTrue(nfs_helper_findings(probe), probe)
    def test_unrecognised_output_fails_closed(self):
        self.assertTrue(nfs_helper_findings('nfs-common installed'))


class DeviceOwnershipTest(unittest.TestCase):
    def test_pool_name_and_uuid_must_both_match(self):
        validate_pool('cubepod-own', 'cubepod-own-pool', 'CUBEPOD_E2E_cubepod-own')
        for name, uuid in [('other-pool', 'CUBEPOD_E2E_cubepod-own'), ('cubepod-own-pool', 'FOREIGN')]:
            with self.assertRaises(AssertionError):
                validate_pool('cubepod-own', name, uuid)

    def test_thin_device_parent_must_be_owned_pool(self):
        validate_thin('253:1', ['0', '1000', 'thin', '253:1', '2'])
        with self.assertRaises(AssertionError):
            validate_thin('253:1', ['0', '1000', 'thin', '253:9', '2'])

    def test_symlink_cannot_select_external_backing_file(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            base = root / 'owned'; base.mkdir()
            own = base / 'data'; own.write_text('owned')
            external = root / 'foreign'; external.write_text('never detach')
            (base / 'link').symlink_to(external)
            self.assertEqual(list(owned_files(base)), [own])


class DevicePublicationTest(unittest.TestCase):
    def test_only_owned_thin_names_parent_and_uuid_are_accepted(self):
        cluster = 'cubepod-fc-own'
        name = cluster + '-pool-snap-1'
        fields = ['0', '2048', 'thin', '253:1', '1']
        uuid = 'CUBEPOD_E2E_' + cluster
        validate_publication(cluster, name, uuid, fields, '253:1')
        for candidate, owner, table in [
            ('host-disk', uuid, fields),
            (name, 'FOREIGN_UUID', fields),
            (name, uuid, ['0', '2048', 'thin', '253:9', '1']),
            (name, uuid, ['0', '2048', 'linear', '253:1', '1']),
        ]:
            with self.assertRaises(AssertionError):
                validate_publication(cluster, candidate, owner, table, '253:1')


    def test_publication_waits_for_actual_kernel_ownership(self):
        cluster = 'cubepod-fc-own'
        pool = cluster + '-pool'
        name = pool + '-snap-1'
        path = MagicMock()
        path.__truediv__.return_value = path
        path.read_text.return_value = pool
        path.is_symlink.return_value = False
        path.exists.return_value = False
        path.stat.return_value = SimpleNamespace(st_mode=stat.S_IFBLK, st_rdev=os.makedev(253, 2))
        answers = ['CUBEPOD_E2E_' + cluster, '253:1', 'CUBEPOD_E2E_' + cluster,
                   '0 2048 thin 253:1 1', '253:2', 'dm-2']
        with patch.object(dmsetup_node, 'Path', return_value=path), \
             patch.object(dmsetup_node, 'query', side_effect=answers), \
             patch.object(dmsetup_node.subprocess, 'run') as create, \
             patch.object(dmsetup_node.os, 'mknod') as publish:
            dmsetup_node.main(cluster, ['create', name, '--table', '0 2048 thin /dev/mapper/' + pool + ' 1'])
            create.assert_called_once()
            self.assertEqual(publish.call_count, 2)
            for call in publish.call_args_list:
                self.assertEqual(call.args, (path, stat.S_IFBLK | 0o600, os.makedev(253, 2)))

    def test_foreign_request_never_mutates_or_publishes(self):
        cluster = 'cubepod-fc-own'
        path = MagicMock(); path.__truediv__.return_value = path
        path.read_text.return_value = cluster + '-pool'
        with patch.object(dmsetup_node, 'Path', return_value=path), \
             patch.object(dmsetup_node, 'query', return_value='CUBEPOD_E2E_' + cluster), \
             patch.object(dmsetup_node.subprocess, 'run') as create, \
             patch.object(dmsetup_node.os, 'mknod') as publish:
            with self.assertRaises(AssertionError):
                dmsetup_node.main(cluster, ['create', 'foreign-disk', '--table', '0 2048 thin /dev/mapper/' + cluster + '-pool 1'])
            create.assert_not_called()
            publish.assert_not_called()


class DiagnosticOwnershipTest(unittest.TestCase):
    def test_diagnostics_reject_foreign_or_stale_devices(self):
        cluster = 'cubepod-fc-owned'
        name = cluster + '-pool-snap-12'
        uuid = 'CUBEPOD_E2E_' + cluster
        validate_diagnostic(cluster, name, uuid, '0 2048 thin 253:1 12', '253:1')
        for candidate, owner, table in [
            ('sda', uuid, '0 2048 thin 253:1 12'),
            (name, 'FOREIGN', '0 2048 thin 253:1 12'),
            (name, uuid, '0 2048 thin 253:9 12'),
            (name, uuid, '0 2048 linear 253:1 12'),
        ]:
            with self.assertRaises(AssertionError):
                validate_diagnostic(cluster, candidate, owner, table, '253:1')


if __name__ == '__main__':
    unittest.main()
