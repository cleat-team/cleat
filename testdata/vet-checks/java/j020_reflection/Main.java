// cleat#1812. Reflection is undecidable for static analysis in general --
// Class.forName with a COMPUTED name cannot be resolved by any resolver,
// bytecode included -- so the rule is to forbid the entry point outright.
// This fixture uses a literal name specifically so the checker's job here is
// bounded: recognise the call, not evaluate its argument.
public class Main {
    public static Object workflow(String which) throws Exception {
        return Class.forName(which).getDeclaredConstructor().newInstance();
    }
}
