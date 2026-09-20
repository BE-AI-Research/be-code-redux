// netstandard2.0 predates C# 9 records/init accessors at the runtime level:
// the compiler emits a reference to System.Runtime.CompilerServices.IsExternalInit
// for any `init` property, and expects to find the type, not define it. This is
// the standard polyfill (an empty marker type) that lets `record`/`init` compile
// against netstandard2.0. It carries no behaviour of its own.
namespace System.Runtime.CompilerServices
{
    internal static class IsExternalInit
    {
    }
}
