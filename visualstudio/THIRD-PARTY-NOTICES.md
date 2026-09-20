# Third-party notices

The BE-Code Visual Studio extension (`BECode.VisualStudio.vsix`) redistributes the following .NET libraries, unmodified, as published by Microsoft on NuGet. They are carried so that the bridge can load inside `devenv.exe` on a Visual Studio that does not itself provide one of them; where Visual Studio provides its own copy, Visual Studio's is the one that loads.

| Assembly | NuGet package |
|---|---|
| `System.Text.Json.dll` | System.Text.Json 6.0.x |
| `System.Text.Encodings.Web.dll` | System.Text.Encodings.Web 6.0.x |
| `System.Threading.Channels.dll` | System.Threading.Channels 6.0.x |
| `Microsoft.Bcl.AsyncInterfaces.dll` | Microsoft.Bcl.AsyncInterfaces 6.0.x |
| `System.Memory.dll` | System.Memory 4.5.x |
| `System.Buffers.dll` | System.Buffers 4.5.x |
| `System.Numerics.Vectors.dll` | System.Numerics.Vectors 4.5.x |
| `System.Runtime.CompilerServices.Unsafe.dll` | System.Runtime.CompilerServices.Unsafe 6.0.x |
| `System.Threading.Tasks.Extensions.dll` | System.Threading.Tasks.Extensions 4.5.x |
| `System.ValueTuple.dll` | System.ValueTuple 4.5.x |

All are part of .NET (https://github.com/dotnet/runtime, and its predecessor https://github.com/dotnet/corefx) and are licensed under the MIT License:

```
The MIT License (MIT)

Copyright (c) .NET Foundation and Contributors

All rights reserved.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

The extension is compiled against, but does not redistribute, the Visual Studio SDK and the .NET Compiler Platform ("Roslyn"); both are provided by Visual Studio at run time.
