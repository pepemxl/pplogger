PY := python3.10
VENV := venv
REPONAME := $(notdir $(CURDIR))

.PHONY: help
help:
	@echo "pplogger — available targets:"
	@echo "  make install   Create venv and install the project with Poetry"
	@echo "  make test       Run the Python test suite"
	@echo "  make lint       Run mypy type checks"
	@echo "  make processor  Build the Go log shipper"
	@echo "  make clean      Remove venv and build artifacts"

${VENV}:
	@echo "Creating venv"
	${PY} -m venv ./${VENV}
	@echo "Upgrading pip and installing Poetry"
	./${VENV}/bin/python3 -m pip install --upgrade pip
	./${VENV}/bin/pip install poetry
	./${VENV}/bin/poetry install

.PHONY: install
install: ${VENV}
	@echo "Installed ${REPONAME} in virtual environment."
	@echo "Linux: activate with \"source ${VENV}/bin/activate\""

.PHONY: test
test: ${VENV}
	./${VENV}/bin/poetry run pytest

.PHONY: lint
lint: ${VENV}
	./${VENV}/bin/poetry run mypy pplogger

.PHONY: processor
processor:
	cd processor && go build -o pplogger-processor

.PHONY: clean
clean:
	rm -rf dist
	rm -rf ${VENV}
	rm -f poetry.lock
	rm -f processor/pplogger-processor
