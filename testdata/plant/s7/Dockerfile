# The S7 server the S7 tests talk to: python-snap7 3.2.1 (MIT), whose server is
# written in pure Python and speaks the S7 protocol itself. Nothing from Snap7's
# own C library is in it.
FROM docker.io/library/python:3.13-slim
RUN pip install --no-cache-dir python-snap7==3.2.1
COPY s7_server.py /s7_server.py
EXPOSE 102
CMD ["python", "-u", "/s7_server.py"]
