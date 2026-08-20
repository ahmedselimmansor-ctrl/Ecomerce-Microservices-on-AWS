# syntax=docker/dockerfile:1.7
#
# BUILD CONTEXT IS THE REPOSITORY ROOT:
#
#     docker build -f services/search-service/Dockerfile .
#
# And the repo layout is REPLICATED under /repo, for the same reason as the Node
# images: requirements.txt records the shared library as
# `-e ../../libs/py-contracts`, a path relative to the requirements file. Flatten
# the layout and pip resolves it to nothing.

FROM python:3.12-slim AS build
WORKDIR /repo
COPY libs/py-contracts ./libs/py-contracts
COPY services/search-service/requirements.txt ./services/search-service/
WORKDIR /repo/services/search-service
# --prefix rather than a venv: the runtime stage copies /install over
# /usr/local and there is no activation step to get wrong.
RUN pip install --no-cache-dir --prefix=/install -r requirements.txt

FROM python:3.12-slim AS runtime
RUN useradd --create-home --uid 1000 souq
WORKDIR /app
COPY --from=build /install /usr/local
# souq_contracts is installed editable, so the package directory itself has to
# exist at runtime — the .pth file in site-packages points at it by absolute
# path, which is why the layout has to match the build stage exactly.
COPY --from=build --chown=souq:souq /repo/libs/py-contracts /repo/libs/py-contracts
COPY --chown=souq:souq services/search-service/app ./app
USER souq
EXPOSE 8087
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
CMD ["python","-m","uvicorn","app.main:app","--host","0.0.0.0","--port","8087"]
