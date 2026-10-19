"""An S7 server for the S7 read and write tests.

python-snap7 3.2.1 (MIT) ships a server written in pure Python that speaks the
S7 protocol over ISO on TCP. This script registers a few memory areas with known
contents and logs every read and write it is asked for, so what a test did can
be read back from the container's output.
"""

import logging
import struct
import sys
import time

from snap7.server import Server
from snap7.type import SrvArea

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s", stream=sys.stdout)
log = logging.getLogger("s7sim")

db1 = bytearray(256)
struct.pack_into(">h", db1, 0, 1234)  # DB1.DBW0 INT
struct.pack_into(">i", db1, 2, -70000)  # DB1.DBD2 DINT
struct.pack_into(">f", db1, 6, 12.5)  # DB1.DBD6 REAL
db1[10] = 0b00000101  # DB1.DBX10.0 and DB1.DBX10.2 are on
db1[11] = 200  # DB1.DBB11 BYTE
struct.pack_into(">H", db1, 12, 65000)  # DB1.DBW12 UINT
struct.pack_into(">I", db1, 14, 4000000000)  # DB1.DBD14 UDINT
struct.pack_into(">d", db1, 18, 3.141592653589793)  # DB1.DBD18 LREAL
struct.pack_into(">q", db1, 26, -5000000000)  # DB1.DBD26 LINT
db1[34] = 20  # DB1.DBB34 STRING[20], max length
db1[35] = 5  # actual length
db1[36:41] = b"hello"
for i, v in enumerate([10, 20, 30, 40, 50]):  # DB1.DBW56 INT[5]
    struct.pack_into(">h", db1, 56 + 2 * i, v)

db2 = bytearray(64)  # scratch, all zero, for the write tests
mk = bytearray(64)
struct.pack_into(">h", mk, 2, 4321)  # MW2
mk[5] = 0b00000010  # M5.1
pe = bytearray(16)
pe[0] = 0x0F  # IB0
pa = bytearray(16)

READ, WRITE = 0x00020000, 0x00040000


def on_event(event):
    # event_text() carries the area, the offset and the length of the request.
    log.info("%s", server.event_text(event))


server = Server()
server.register_area(SrvArea.DB, 1, db1)
server.register_area(SrvArea.DB, 2, db2)
server.register_area(SrvArea.MK, 0, mk)
server.register_area(SrvArea.PE, 0, pe)
server.register_area(SrvArea.PA, 0, pa)
server.set_events_callback(on_event)
server.set_read_events_callback(on_event)
server.start(tcp_port=102)
log.info("S7 server listening on 102 with DB1 (256 bytes), DB2 (64), M (64), I (16), Q (16)")
while True:
    time.sleep(3600)
