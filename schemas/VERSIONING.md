# Versionado de contratos

`apiVersion` identifica una edición exacta del conjunto de contratos. En
`doppels.so/v1alpha1`, cada kind tiene un `$id` estable bajo
`https://doppels.so/schemas/v1alpha1/`.

Mientras una versión solo exista en este repositorio y no se haya publicado,
puede evolucionar. Desde su primera publicación pasa a ser inmutable:

1. No se modifica el contenido servido por un `$id` publicado.
2. Un cambio de reglas se publica con otra `apiVersion`, incluso si parece
   compatible.
3. La CLI distribuye los schemas y registra su `$id` y el SHA-256 de todo su
   bundle transitivo; validar no depende de descargar reglas cambiantes.
4. Request y Run fijan las revisiones y digests de Capability y Recipe usados.
5. Run conserva además la referencia exacta al schema; RunEvent nunca reescribe
   el historial.

Por ejemplo, una evolución posterior se publicaría como
`doppels.so/v1alpha2`, manteniendo accesible e intacta `v1alpha1`.

La versión de una Capability o Recipe (`metadata.version`) y `apiVersion` no
son lo mismo: la primera versiona una definición de usuario; la segunda
versiona el lenguaje y sus reglas de validación. Space no lleva
`metadata.version`, porque expresa configuración mutable; el control plane
conserva por separado sus cambios y las revisiones inmutables que contiene.

## Digest del bundle de schema

`SchemaReference.sha256` no es el digest aislado del fichero raíz. Usa el
algoritmo versionado `doppels.so/schema-bundle-sha256/v1`:

1. Se parte del recurso identificado por `SchemaReference.id` y se resuelve la
   clausura transitiva de cada `$ref` externo. El fragmento no forma parte del
   identificador del recurso.
2. Cada recurso debe estar fijado localmente por sus bytes exactos y su `$id`.
   Una referencia externa ausente o un ciclo entre recursos invalida el
   bundle; nunca se descarga contenido durante la validación.
3. Los recursos únicos se ordenan por los bytes UTF-8 de `$id`.
4. Para cada recurso se calcula `SHA-256(sourceBytes)`, donde `sourceBytes` son
   los bytes exactos publicados, sin canonicalizar JSON ni finales de línea.
5. Se construye este stream binario (`u64be` es un entero sin signo de 64 bits
   en big-endian):

   ```text
   "doppels.so/schema-bundle-sha256/v1\0"
   || u64be(resourceCount)
   || por cada recurso:
        u64be(len(idUtf8)) || idUtf8
        || u64be(32) || SHA-256(sourceBytes)
   ```

6. `SchemaReference.sha256` es el SHA-256 final del stream, codificado como 64
   caracteres hexadecimales minúsculos.

El namespace, orden, delimitación y algoritmo pasan a ser inmutables al
publicar la `apiVersion`. Cambiarlos exige otra versión del contrato y otro
namespace. El validador embebido compila exactamente la misma clausura de
recursos usada para calcular este digest.

Dentro de `v1alpha1`, `metadata.version` admite como máximo 100 caracteres. El
límite coincide en schema, CLI y persistencia del control plane para que un
manifiesto aceptado localmente no falle al registrarse.
