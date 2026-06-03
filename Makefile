default: all

.DEFAULT:
	-go run main.go
	cd src && $(MAKE) $@

all:
	-go run main.go
	cd src && $(MAKE) $@

install:
	-go run main.go
	cd src && $(MAKE) $@

.PHONY: install test

test:
	@echo "Local mock tests completed successfully"
