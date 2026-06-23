package sample;

import static org.junit.jupiter.api.Assertions.assertEquals;
import org.junit.jupiter.api.Test;

public class AppTest {
    @Test
    void summaryCombinesDaypartAndGrade() {
        // hour 9 -> "morning", score 60 -> "pass"
        assertEquals("morning/pass", new App().summary(9, 60));
    }
}
